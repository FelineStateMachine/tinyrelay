package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
)

const mcpGrantRequestShape = `Expected a signed kind 1111 NIP-22 event with matching E/K/P and e/k tags for the current kind 30392 grant, one p tag for its operator, request=grant and a grant JSON tag containing the current grant id and additive changes.`

func mcpGrantRequestSchema() map[string]any {
	text := map[string]any{"type": "string", "minLength": 1}
	repo := mcp.Object(map[string]any{
		"owner": mcpPubKey, "identifier": text,
		"level": map[string]any{"type": "string", "enum": []string{community.AgentRepoPropose, community.AgentRepoRead, community.AgentRepoMaintain}},
	}, "owner", "identifier", "level")
	site := mcp.Object(map[string]any{"label": text, "ttl": map[string]any{"type": "integer", "minimum": 1, "maximum": community.AgentSiteTTLMax}, "encrypted": map[string]any{"type": "boolean"}}, "label")
	changes := mcp.Object(map[string]any{
		"kinds": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 0, "maximum": 65535}},
		"rooms": map[string]any{"type": "array", "items": text}, "repos": map[string]any{"type": "array", "items": repo},
		"sites": map[string]any{"type": "array", "items": site}, "wiki": text, "jobs": text,
		"rate": map[string]any{"type": "integer", "minimum": 1, "maximum": community.AgentRateMax},
	})
	return mcp.Object(map[string]any{"event": mcpEvent, "reason": map[string]any{"type": "string", "minLength": 1, "maxLength": 500}, "changes": changes})
}

func (t *Tenant) mcpGrantRequest(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	if raw, present := call.Arguments["event"]; present && raw != nil {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return mcp.Result{}, err
		}
		e, err := event.Parse(encoded)
		if err == nil && e.PubKey != call.Actor {
			err = errors.New("the event pubkey must match the authenticated agent")
		}
		if err == nil {
			_, err = t.community.ReviewAgentGrantRequest(ctx, e, time.Now().Unix())
		}
		if err != nil {
			return mcp.Failure("The grant request is not acceptable: "+err.Error()+"\n"+mcpGrantRequestShape, map[string]any{"expected": mcpGrantRequestShape}), nil
		}
		return t.mcpPublish(ctx, call, e), nil
	}
	unsigned, err := t.mcpBuildGrantRequest(ctx, call)
	if err != nil {
		return mcp.Failure(err.Error()+"\n"+mcpGrantRequestShape, map[string]any{"expected": mcpGrantRequestShape}), nil
	}
	return mcp.Value(map[string]any{"unsigned": unsigned, "next": mcpSignNext}), nil
}

func (t *Tenant) mcpBuildGrantRequest(ctx context.Context, call mcp.Call) (mcpUnsigned, error) {
	reason := strings.TrimSpace(call.String("reason"))
	if reason == "" || !utf8.ValidString(reason) || len([]rune(reason)) > 500 {
		return mcpUnsigned{}, errors.New("reason must be 1 to 500 valid UTF-8 characters")
	}
	grant, ok, err := t.community.AgentGrant(ctx, call.Actor)
	if err != nil {
		return mcpUnsigned{}, err
	}
	if !ok || !grant.Active(time.Now().Unix()) {
		return mcpUnsigned{}, errors.New("only an active granted agent can request more permissions")
	}
	changes, err := mcpGrantChanges(call.Arguments["changes"])
	if err != nil {
		return mcpUnsigned{}, err
	}
	grantJSON, err := json.Marshal(community.AgentGrantRequest{Base: grant.EventID, Changes: changes})
	if err != nil {
		return mcpUnsigned{}, err
	}
	tags := [][]string{{"E", grant.EventID, "", grant.Owner}, {"K", "30392"}, {"P", grant.Owner}, {"e", grant.EventID, "", grant.Owner}, {"k", "30392"}, {"request", "grant"}, {"grant", string(grantJSON)}, {"p", grant.Owner}}
	unsigned := mcpUnsigned{Kind: 1111, CreatedAt: time.Now().Unix(), Tags: tags, Content: reason}
	request := event.Event{PubKey: call.Actor, Kind: unsigned.Kind, CreatedAt: unsigned.CreatedAt, Tags: unsigned.Tags, Content: unsigned.Content}
	if _, err := t.community.ReviewAgentGrantRequest(ctx, request, time.Now().Unix()); err != nil {
		return mcpUnsigned{}, err
	}
	return unsigned, nil
}

func mcpGrantChanges(raw any) (community.AgentGrantChanges, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return community.AgentGrantChanges{}, errors.New("changes must be an object")
	}
	var changes community.AgentGrantChanges
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&changes); err != nil {
		return community.AgentGrantChanges{}, errors.New("changes must contain only supported grant fields")
	}
	if changes.Kinds == nil && changes.Rooms == nil && changes.Repos == nil && changes.Sites == nil && changes.Wiki == nil && changes.Jobs == nil && changes.Rate == nil {
		return community.AgentGrantChanges{}, errors.New("changes must request at least one permission")
	}
	return changes, nil
}
