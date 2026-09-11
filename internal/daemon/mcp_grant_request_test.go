package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
)

func TestMCPGrantRequestBuildsOperatorBoundRequest(t *testing.T) {
	_, tenant := testTenant(t)
	agent, _ := event.PublicKey(testAgentSecret)
	now := time.Now().Unix()
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now, now+3600, []string{"k", "1111"})); err != nil {
		t.Fatal(err)
	}
	changes := map[string]any{"kinds": []any{9}, "rooms": []any{"work"}}
	unsigned, err := tenant.mcpBuildGrantRequest(context.Background(), mcp.Call{Actor: agent, Arguments: map[string]any{"reason": "Need chat replies", "changes": changes}})
	if err != nil {
		t.Fatal(err)
	}
	if unsigned.Kind != 1111 || unsigned.Content != "Need chat replies" {
		t.Fatalf("unsigned request: %+v", unsigned)
	}
	if event.Tag(event.Event{Tags: unsigned.Tags}, "p") != tenant.Policy().Owner || event.Tag(event.Event{Tags: unsigned.Tags}, "request") != "grant" {
		t.Fatalf("request is not bound to operator: %v", unsigned.Tags)
	}
	grant := event.Tag(event.Event{Tags: unsigned.Tags}, "grant")
	if !strings.Contains(grant, `"base"`) || !strings.Contains(grant, `"kinds":[9]`) {
		t.Fatalf("grant change payload: %s", grant)
	}
}

func TestMCPGrantRequestRejectsUnsupportedChanges(t *testing.T) {
	for _, raw := range []any{map[string]any{"unknown": "x"}, map[string]any{}} {
		if _, err := mcpGrantChanges(raw); err == nil {
			t.Errorf("accepted invalid changes: %#v", raw)
		}
	}
}

func TestMCPGrantRequestRejectsMissingGrantAndLongReason(t *testing.T) {
	_, tenant := testTenant(t)
	agent, _ := event.PublicKey(testAgentSecret)
	call := mcp.Call{Actor: agent, Arguments: map[string]any{"reason": "need access", "changes": map[string]any{"kinds": []any{float64(9)}}}}
	if _, err := tenant.mcpBuildGrantRequest(context.Background(), call); err == nil {
		t.Fatal("agent without a grant accepted")
	}
	now := time.Now().Unix()
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now, now+3600, []string{"k", "1111"})); err != nil {
		t.Fatal(err)
	}
	call.Arguments["reason"] = strings.Repeat("x", 501)
	if _, err := tenant.mcpBuildGrantRequest(context.Background(), call); err == nil {
		t.Fatal("501-character reason accepted")
	}
}

func TestMCPGrantRequestSchema(t *testing.T) {
	schema := mcpGrantRequestSchema()
	if err := mcp.Validate(schema, map[string]any{"event": map[string]any{}}); err != nil {
		t.Fatalf("signed follow-up should be schema-valid: %v", err)
	}
	valid := map[string]any{"reason": "Need room chat", "changes": map[string]any{"kinds": []any{float64(9)}}}
	if err := mcp.Validate(schema, valid); err != nil {
		t.Fatal(err)
	}
	if err := mcp.Validate(schema, map[string]any{"reason": "", "changes": map[string]any{"kinds": []any{float64(9)}}}); err == nil {
		t.Fatal("empty reason accepted")
	}
}
