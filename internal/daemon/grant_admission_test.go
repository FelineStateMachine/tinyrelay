package daemon

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func grantRequestEvent(t *testing.T, secret string, grant event.Event, at int64, content, grantJSON string, tags ...[]string) event.Event {
	t.Helper()
	base := [][]string{{"request", "grant"}, {"grant", grantJSON}, {"E", grant.ID, "", grant.PubKey}, {"K", "30392"}, {"P", grant.PubKey}, {"e", grant.ID, "", grant.PubKey}, {"k", "30392"}, {"p", grant.PubKey}}
	return signedEvent(t, secret, 1111, at, append(base, tags...), content)
}

func grantRequestJSON(t *testing.T, grant event.Event, changes community.AgentGrantChanges) string {
	t.Helper()
	raw, err := json.Marshal(community.AgentGrantRequest{Base: grant.ID, Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func setGrantTag(t *testing.T, e *event.Event, name, value string) {
	t.Helper()
	for _, tag := range e.Tags {
		if len(tag) > 1 && tag[0] == name {
			tag[1] = value
			return
		}
	}
	t.Fatalf("grant replacement has no %s tag", name)
}

func TestGrantRequestAdmissionRequiresAgentAndValidCurrentGrant(t *testing.T) {
	_, tenant := testTenant(t)
	now := time.Now().Unix()
	agent, _ := event.PublicKey(testAgentSecret)
	member, _ := event.PublicKey(testMemberSecret)
	grant := agentGrantEvent(t, agent, now-30, now+3600, []string{"k", "9"}, []string{"k", "1111"})
	if err := publishAs(t, tenant, grant); err != nil {
		t.Fatal(err)
	}
	reqJSON := grantRequestJSON(t, grant, community.AgentGrantChanges{Kinds: []int{11}})
	human := grantRequestEvent(t, testMemberSecret, grant, now, "please", reqJSON)
	if err := publishAs(t, tenant, human); err == nil {
		t.Fatal("human request accepted")
	}

	for name, mutate := range map[string]func(event.Event) event.Event{
		"rationale over limit": func(e event.Event) event.Event { e.Content = strings.Repeat("x", 501); return e },
		"rationale whitespace": func(e event.Event) event.Event { e.Content = " \t\n "; return e },
		"duplicate operator":   func(e event.Event) event.Event { e.Tags = append(e.Tags, []string{"p", grant.PubKey}); return e },
		"wrong operator":       func(e event.Event) event.Event { e.Tags[4][1] = member; e.Tags[7][1] = member; return e },
		"wrong base": func(e event.Event) event.Event {
			e.Tags[2][1] = strings.Repeat("f", 64)
			e.Tags[5][1] = strings.Repeat("f", 64)
			return e
		},
		"unknown change": func(e event.Event) event.Event {
			e.Tags[1][1] = `{"base":"` + grant.ID + `","changes":{"nope":true}}`
			return e
		},
		"noop": func(e event.Event) event.Event {
			e.Tags[1][1] = grantRequestJSON(t, grant, community.AgentGrantChanges{Kinds: []int{9}})
			return e
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := grantRequestEvent(t, testAgentSecret, grant, now+1, "please", reqJSON)
			e = mutate(e)
			if err := event.Sign(&e, testAgentSecret); err != nil {
				t.Fatal(err)
			}
			if err := publishAs(t, tenant, e); err == nil {
				t.Fatalf("malformed request accepted: %v", e)
			}
		})
	}
}

func TestGrantRequestAdmissionRejectsPausedAndRevokedAgents(t *testing.T) {
	for _, action := range []string{"pauseagent", "revokeagent"} {
		t.Run(action, func(t *testing.T) {
			_, tenant := testTenant(t)
			now := time.Now().Unix()
			agent, _ := event.PublicKey(testAgentSecret)
			grant := agentGrantEvent(t, agent, now-30, now+3600, []string{"k", "9"})
			if err := publishAs(t, tenant, grant); err != nil {
				t.Fatal(err)
			}
			args := []json.RawMessage{json.RawMessage(`"` + agent + `"`)}
			if _, err := tenant.Execute(context.Background(), tenant.Policy().Owner, action, args); err != nil {
				t.Fatal(err)
			}
			req := grantRequestEvent(t, testAgentSecret, grant, now+1, "please", grantRequestJSON(t, grant, community.AgentGrantChanges{Kinds: []int{11}}))
			if err := publishAs(t, tenant, req); err == nil {
				t.Fatalf("request accepted after %s", action)
			}
		})
	}
}

func TestGrantRequestReplacementMustMatchPreparedReview(t *testing.T) {
	mutations := map[string]func(*event.Event){
		"expiry":  func(e *event.Event) { setGrantTag(t, e, "expiration", "9999999999") },
		"name":    func(e *event.Event) { setGrantTag(t, e, "name", "changed") },
		"content": func(e *event.Event) { e.Content = "changed" },
		"tags":    func(e *event.Event) { e.Tags = append(e.Tags, []string{"note", "changed"}) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			_, tenant := testTenant(t)
			now := time.Now().Unix()
			agent, _ := event.PublicKey(testAgentSecret)
			grant := agentGrantEvent(t, agent, now-30, now+3600, []string{"k", "9"})
			if err := publishAs(t, tenant, grant); err != nil {
				t.Fatal(err)
			}
			req := grantRequestEvent(t, testAgentSecret, grant, now, "please", grantRequestJSON(t, grant, community.AgentGrantChanges{Kinds: []int{11}}))
			if err := publishAs(t, tenant, req); err != nil {
				t.Fatal(err)
			}
			review, err := tenant.community.ReviewAgentGrantRequest(context.Background(), req, now)
			if err != nil {
				t.Fatal(err)
			}
			replacement := review.Unsigned
			mutate(&replacement)
			if err := event.Sign(&replacement, testOwnerSecret); err != nil {
				t.Fatal(err)
			}
			if err := publishAs(t, tenant, replacement); err == nil {
				t.Fatalf("mutated %s replacement accepted", name)
			}
		})
	}
}

func TestGrantRequestPreparedReplacementGoesStale(t *testing.T) {
	for _, scenario := range []string{"pause", "revoke", "deny", "expire"} {
		t.Run(scenario, func(t *testing.T) {
			_, tenant := testTenant(t)
			now := time.Now().Unix()
			agent, _ := event.PublicKey(testAgentSecret)
			grant := agentGrantEvent(t, agent, now-30, now+3600, []string{"k", "9"})
			if err := publishAs(t, tenant, grant); err != nil {
				t.Fatal(err)
			}
			tags := grantRequestJSON(t, grant, community.AgentGrantChanges{Kinds: []int{11}})
			requestTags := [][]string(nil)
			if scenario == "expire" {
				requestTags = [][]string{{"expiration", strconv.FormatInt(now+1, 10)}}
			}
			request := grantRequestEvent(t, testAgentSecret, grant, now, "please", tags, requestTags...)
			if err := publishAs(t, tenant, request); err != nil {
				t.Fatal(err)
			}
			review, err := tenant.community.ReviewAgentGrantRequest(context.Background(), request, now)
			if err != nil {
				t.Fatal(err)
			}
			replacement := review.Unsigned
			if scenario == "deny" {
				if err := publishAs(t, tenant, signedEvent(t, testOwnerSecret, 7, now+1, [][]string{{"e", request.ID}, {"p", agent}}, "-")); err != nil {
					t.Fatal(err)
				}
			} else if scenario != "expire" {
				action := "pauseagent"
				if scenario == "revoke" {
					action = "revokeagent"
				}
				if _, err := tenant.Execute(context.Background(), tenant.Policy().Owner, action, []json.RawMessage{json.RawMessage(`"` + agent + `"`)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := event.Sign(&replacement, testOwnerSecret); err != nil {
				t.Fatal(err)
			}
			if scenario == "expire" {
				time.Sleep(2 * time.Second)
			}
			if err := publishAs(t, tenant, replacement); err == nil {
				t.Fatalf("stale %s replacement accepted", scenario)
			}
		})
	}
}
