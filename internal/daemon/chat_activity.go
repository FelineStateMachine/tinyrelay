package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func (t *Tenant) browseChatActivity(ctx context.Context, actor string, params []json.RawMessage) (any, error) {
	var q struct {
		Room string `json:"room"`
		Root string `json:"root"`
	}
	if len(params) != 1 || json.Unmarshal(params[0], &q) != nil || q.Room == "" {
		return nil, errors.New("invalid: chat activity room")
	}
	if _, _, err := t.roomFor(ctx, actor, q.Room); err != nil {
		return nil, err
	}
	if q.Root != "" {
		root, err := t.roomThreadRoot(ctx, actor, q.Room, q.Root)
		if err != nil {
			return nil, err
		}
		q.Root = root.ID
	}
	rows := []event.Event{}
	for _, filter := range []event.Filter{
		{Kinds: approvalKinds, Tags: map[string][]string{"h": {q.Room}}},
		{Kinds: jobRequestKinds, Tags: map[string][]string{"h": {q.Room}}},
	} {
		page, err := t.store.Query(ctx, filter, storage.QueryOptions{Access: storage.Access{PubKeys: []string{actor}}, Limit: 1000})
		if err != nil {
			return nil, err
		}
		rows = append(rows, page.Events...)
	}
	session := browseSession(t, actor)
	jobs, approvals := []jobItem{}, []approvalItem{}
	now := time.Now().Unix()
	roots := map[string]string{}
	for _, row := range rows {
		_, isApproval := approvalRequest(row)
		if !event.IsJobRequest(row.Kind) && !isApproval {
			continue
		}
		if !t.gate.CanSee(ctx, row, session, nil) {
			continue
		}
		if q.Root != "" && !t.activityInThread(ctx, actor, q.Root, row, roots) {
			continue
		}
		if event.IsJobRequest(row.Kind) && t.Policy().Features.Jobs && (event.Expiration(row) == 0 || event.Expiration(row) > now) {
			jobs = append(jobs, jobItemFrom(row))
		} else if kind, ok := approvalRequest(row); ok && (row.PubKey == actor || containsString(approvalAsked(row), actor)) {
			approvals = append(approvals, approvalItemFrom(row, kind, t.publicURL))
		}
	}
	// Keep the inline panel bounded even when a busy room has accumulated a
	// large amount of historical task and approval traffic. The store returns
	// each filter newest first, so retaining the first 50 preserves the useful
	// recent context while the dedicated views remain available for history.
	if len(jobs) > 50 {
		jobs = jobs[:50]
	}
	if len(approvals) > 50 {
		approvals = approvals[:50]
	}
	answers, err := t.jobAnswers(ctx, jobs, session, now)
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		jobSettle(&jobs[i], answers[jobs[i].ID])
	}
	decisions, err := t.approvalAnswers(ctx, approvals, now)
	if err != nil {
		return nil, err
	}
	for i := range approvals {
		approvalSettle(&approvals[i], decisions[approvals[i].ID], now)
	}
	return map[string]any{"jobs": jobs, "approvals": approvals}, nil
}

func (b backend) ReadChatActivity(ctx context.Context, actor, room, root string) (any, error) {
	raw, err := json.Marshal(map[string]string{"room": room, "root": root})
	if err != nil {
		return nil, err
	}
	return b.tenant.Execute(ctx, actor, "browsechatactivity", []json.RawMessage{raw})
}

// Resolve parent-only references once per parent while keeping unrelated tasks
// out of the thread. roomThreadRoot also enforces room and reader access.
func (t *Tenant) activityInThread(ctx context.Context, actor, root string, row event.Event, roots map[string]string) bool {
	if row.ID == root {
		return true
	}
	reference := event.RoomReplyRoot(row)
	if reference == "" && event.IsJobRequest(row.Kind) {
		reference = event.Tag(row, "e")
	}
	if reference == root {
		return true
	}
	if reference == "" {
		return false
	}
	canonical, known := roots[reference]
	if !known {
		parent, err := t.roomThreadRoot(ctx, actor, event.Tag(row, "h"), reference)
		if err == nil {
			canonical = parent.ID
		}
		roots[reference] = canonical
	}
	return canonical == root
}
