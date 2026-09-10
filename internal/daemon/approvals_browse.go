package daemon

// Approval browsing: the requests addressed to the caller with their state
// and counts, and one request with every answer it received. Both read
// through the tenant gate with the caller's session, like the other browse
// methods.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type approvalBrowseRequest struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
	State  string `json:"state"`
	ID     string `json:"id"`
	Event  string `json:"event"`
}

// approvalDevice is one of the caller's registered devices, as a fact for
// the panel: when it was registered and which categories it wakes for.
type approvalDevice struct {
	CreatedAt  int64    `json:"created_at"`
	Categories []string `json:"categories"`
}

func approvalBrowseMethod(method string) bool {
	return method == "browseapprovals" || method == "browseapproval"
}

func (t *Tenant) executeApprovals(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
	if actor == "" {
		return nil, errors.New("auth-required: sign in to see requests addressed to you")
	}
	q := approvalBrowseRequest{}
	if len(params) > 0 {
		if err := json.Unmarshal(params[0], &q); err != nil {
			return nil, fmt.Errorf("invalid: approval parameters: %w", err)
		}
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 50
	}
	switch method {
	case "browseapprovals":
		return t.browseApprovals(ctx, actor, q)
	case "browseapproval":
		if q.ID == "" {
			q.ID = q.Event
		}
		return t.browseApproval(ctx, actor, q.ID)
	}
	return nil, errors.New("unsupported: browse operation")
}

// browseApprovals lists the requests addressed to the caller. The counts
// cover every request the walk saw, so the panel can say how many wait even
// when the page shows only one state.
func (t *Tenant) browseApprovals(ctx context.Context, actor string, q approvalBrowseRequest) (any, error) {
	if q.State == "" {
		q.State = "all"
	}
	if !containsString([]string{"open", "answered", "expired", "all"}, q.State) {
		return nil, errors.New("invalid: approval state")
	}
	cursor, err := parseCollaborationCursor(q.Cursor)
	if err != nil {
		return nil, errors.New("invalid: approval cursor")
	}
	session := browseSession(t, actor)
	items := make([]approvalItem, 0, q.Limit)
	counts := map[string]int{"open": 0, "answered": 0, "expired": 0}
	var oldest int64
	next := ""
	for scanned := 0; scanned < approvalScan; scanned += 100 {
		page, more, err := t.approvalsFor(ctx, actor, cursor, 100)
		if err != nil {
			return nil, err
		}
		for _, item := range page {
			cursor = &storage.EventCursor{CreatedAt: item.CreatedAt, ID: item.ID}
			if !t.gate.CanSee(ctx, item.Event, session, nil) {
				continue
			}
			counts[item.State]++
			if item.State == "open" && (oldest == 0 || item.CreatedAt < oldest) {
				oldest = item.CreatedAt
			}
			if q.State != "all" && q.State != item.State {
				continue
			}
			if len(items) < q.Limit {
				items = append(items, item)
			} else if next == "" {
				next = approvalCursor(items[len(items)-1])
			}
		}
		if !more || len(page) == 0 {
			break
		}
		if next == "" && len(items) == q.Limit {
			next = approvalCursor(items[len(items)-1])
		}
	}
	devices, err := t.approvalDevices(ctx, actor)
	if err != nil {
		return nil, err
	}
	return map[string]any{"items": items, "next_cursor": next, "counts": counts, "oldest_open": oldest, "devices": devices}, nil
}

// browseApproval returns one request with every reaction and reply from the
// asked keys, newest first. The asked people and the asker may read it.
func (t *Tenant) browseApproval(ctx context.Context, actor, id string) (any, error) {
	if len(id) != 64 {
		return nil, errors.New("invalid: approval id")
	}
	if item, ok, err := t.wikiApprovalByID(ctx, actor, id); err != nil {
		return nil, err
	} else if ok {
		answers, err := t.approvalAnswers(ctx, []approvalItem{item}, time.Now().Unix())
		if err != nil {
			return nil, err
		}
		approvalSettle(&item, answers[id], time.Now().Unix())
		if item.Type == approvalWikiProposal {
			if err := t.settleWikiProposal(ctx, &item, t.wikiRoles()); err != nil {
				return nil, err
			}
		} else if item.Type == approvalWikiMerge {
			status, err := t.wikiMergeStatus(ctx, item.ID, actor)
			if err != nil {
				return nil, err
			}
			if status.Status == wikiMergeOpen {
				item.State = "open"
				item.Answer = nil
			} else {
				item.State = "answered"
				item.Answer, err = t.wikiReactionAnswer(ctx, status.Reaction)
				if err != nil {
					return nil, err
				}
			}
		}
		history := answers[id]
		if history == nil {
			history = []event.Event{}
		}
		return map[string]any{"item": item, "answers": history}, nil
	}
	session := browseSession(t, actor)
	rows, err := t.Query(ctx, []event.Filter{{IDs: []string{id}, Kinds: approvalKinds, Limit: intPtr(1)}}, session)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, errors.New("not found: request")
	}
	kind, ok := approvalRequest(rows[0])
	if !ok {
		return nil, errors.New("not found: request")
	}
	item := approvalItemFrom(rows[0], kind, strings.TrimRight(t.publicURL, "/"))
	if item.Asker != actor && !containsString(item.Asked, actor) {
		return nil, errors.New("restricted: this request is not addressed to you")
	}
	now := time.Now().Unix()
	answers, err := t.approvalAnswers(ctx, []approvalItem{item}, now)
	if err != nil {
		return nil, err
	}
	approvalSettle(&item, answers[item.ID], now)
	history := answers[item.ID]
	if history == nil {
		history = []event.Event{}
	}
	return map[string]any{"item": item, "answers": history}, nil
}

// approvalDevices lists the caller's registered devices with the categories
// each one chose. Endpoints and keys stay out of the result.
func (t *Tenant) approvalDevices(ctx context.Context, pubkey string) ([]approvalDevice, error) {
	rows, err := t.store.DB().QueryContext(ctx, "SELECT created_at, categories FROM web_push WHERE pubkey=? ORDER BY created_at", pubkey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := []approvalDevice{}
	for rows.Next() {
		var device approvalDevice
		var raw string
		if err := rows.Scan(&device.CreatedAt, &raw); err != nil {
			return nil, err
		}
		device.Categories = pushCategories
		if allowed := decodeCategories(raw); allowed != nil {
			device.Categories = []string{}
			for _, category := range pushCategories {
				if allowed[category] {
					device.Categories = append(device.Categories, category)
				}
			}
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}
