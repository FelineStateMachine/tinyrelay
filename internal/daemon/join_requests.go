package daemon

import (
	"context"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// An access request is a NIP-43 join without a claim. The community service
// holds it for review; this file wakes the reviewers once per new request.

// joinRequestNotices names the owner and moderators as the people asked,
// with the asker's short key and reason in the approvals category.
func (t *Tenant) joinRequestNotices(ctx context.Context, e event.Event) []pushNotice {
	reason := excerpt(community.JoinReason(e.Content))
	if reason == "" {
		reason = "no reason given"
	}
	body := shortKey(e.PubKey) + " asks to join: " + reason
	url := strings.TrimRight(t.publicURL, "/") + "/manage/people"
	recipients := []string{t.Policy().Owner}
	if moderators, err := t.community.Moderators(ctx); err == nil {
		recipients = append(recipients, moderators...)
	}
	notices := make([]pushNotice, 0, len(recipients))
	for _, recipient := range recipients {
		if recipient == e.PubKey {
			continue
		}
		notices = append(notices, pushNotice{recipient: recipient, category: pushApprovals, body: body, url: url})
	}
	return notices
}

// notifyJoinRequest wakes the reviewers' devices for a new pending request.
// A refreshed request never reaches here, so each request wakes them once.
// The log carries counts only.
func (t *Tenant) notifyJoinRequest(ctx context.Context, e event.Event) {
	notices := t.joinRequestNotices(ctx, e)
	t.app.telemetry.Logger().Info("access request", "tenant", t.meta.Name, "reviewers", len(notices))
	if !t.pushDevicesExist(ctx) {
		return
	}
	queued := 0
	for _, notice := range notices {
		if !t.pushCoalesced(notice.recipient, notice.category) {
			continue
		}
		if err := t.enqueuePushNotice(ctx, notice); err != nil {
			t.app.telemetry.Logger().Debug("access request notification not queued", "error", err)
			continue
		}
		queued++
	}
	t.app.telemetry.Logger().Debug("access request notifications queued", "count", queued)
}
