package daemon

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// Keep the decision visible after the addressable grant has been superseded
// or expired. Only archived grants tied to these exact requests are read.
func (t *Tenant) grantApprovalHistory(ctx context.Context, items []approvalItem) ([]event.Event, error) {
	ids, authors := []string{}, []string{}
	for _, item := range items {
		if item.Type == "grant" {
			ids = append(ids, item.ID)
			authors = append(authors, item.Asked...)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	idJSON, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	authorJSON, err := json.Marshal(uniqueStrings(authors))
	if err != nil {
		return nil, err
	}
	rows, err := t.store.DB().QueryContext(ctx, `SELECT raw FROM (
		SELECT raw,created_at FROM events WHERE kind=30392 AND pubkey IN (SELECT value FROM json_each(?))
		UNION ALL SELECT raw,created_at FROM agent_grant_revisions WHERE author IN (SELECT value FROM json_each(?))
	) WHERE EXISTS (SELECT 1 FROM json_each(json_extract(raw,'$.tags')) AS tag
		WHERE json_extract(tag.value,'$[0]')='grant-request' AND json_extract(tag.value,'$[1]') IN (SELECT value FROM json_each(?)))
	ORDER BY created_at DESC LIMIT ?`, string(authorJSON), string(authorJSON), string(idJSON), approvalScan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []event.Event
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		grant, err := event.Parse([]byte(raw))
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// A grant is approved only by the operator's replacement grant. Reactions
// cannot confer permissions or make an unapplied request look successful.
func settleGrantApproval(item *approvalItem, rows []event.Event, now int64) {
	for _, row := range rows {
		if row.Kind != event.KIND_AGENT_GRANT || row.PubKey == item.Asker || !containsString(item.Asked, row.PubKey) || event.Tag(row, "grant-request") != item.ID || event.Tag(row, "grant-base") != event.Tag(item.Event, "E") || event.Tag(row, "d") != item.Asker {
			continue
		}
		item.State = "answered"
		item.Answer = &approvalAnswer{ID: row.ID, Kind: row.Kind, Author: row.PubKey, Decision: "approved", CreatedAt: row.CreatedAt}
		return
	}
	for _, row := range rows {
		if row.Kind != kindReaction || strings.TrimSpace(row.Content) != "-" || !containsString(item.Asked, row.PubKey) {
			continue
		}
		item.State = "answered"
		item.Answer = &approvalAnswer{ID: row.ID, Kind: row.Kind, Author: row.PubKey, Decision: "denied", CreatedAt: row.CreatedAt}
		return
	}
	if item.Expires > 0 && item.Expires <= now {
		item.State = "expired"
	}
}

func (t *Tenant) reviewGrantApproval(ctx context.Context, item *approvalItem, now int64) {
	if item.Type != "grant" || item.State != "open" {
		return
	}
	review, err := t.community.ReviewAgentGrantRequest(ctx, item.Event, now)
	if err != nil {
		item.GrantError = err.Error()
		if strings.HasPrefix(item.GrantError, "conflict:") {
			item.State = "expired"
		}
		return
	}
	item.Grant = &review
}
