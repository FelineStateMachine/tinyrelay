package daemon

// Device notification triggers. Each stored event that addresses a member
// with registered devices becomes a short summary in one of five categories,
// which the person picks per device: private messages, replies to their own
// posts and repositories, mentions, requests for a decision, and relay
// notices.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const (
	pushMessages   = "messages"
	pushReplies    = "replies"
	pushMentions   = "mentions"
	pushApprovals  = "approvals"
	pushRelay      = "relay"
	pushCoalesce   = 30 * time.Second
	pushExcerptMax = 120
)

var pushCategories = []string{pushMessages, pushReplies, pushMentions, pushApprovals, pushRelay}

type pushNotice struct {
	recipient, category, body, url string
	// actions are the answers a device can give from the notification.
	actions []pushAction
}

// notifyDevices runs after an event is stored. It never fails the publish:
// a device summary is best effort on top of the durable event.
func (t *Tenant) notifyDevices(ctx context.Context, e event.Event) {
	if !t.pushDevicesExist(ctx) {
		return
	}
	for _, notice := range t.pushNotices(ctx, e) {
		if !t.pushCoalesced(notice.recipient, notice.category) {
			continue
		}
		if err := t.enqueuePushNotice(ctx, notice); err != nil {
			t.app.telemetry.Logger().Debug("device notification not queued", "error", err)
		}
	}
}

func (t *Tenant) pushDevicesExist(ctx context.Context) bool {
	var count int
	return t.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM web_push").Scan(&count) == nil && count > 0
}

// pushCoalesced allows one summary per recipient and category within a short
// window, so a burst of replies produces a single notification.
func (t *Tenant) pushCoalesced(recipient, category string) bool {
	t.pushMu.Lock()
	defer t.pushMu.Unlock()
	if t.pushRecent == nil {
		t.pushRecent = make(map[string]time.Time)
	}
	key := recipient + "\x00" + category
	now := time.Now()
	if last, ok := t.pushRecent[key]; ok && now.Sub(last) < pushCoalesce {
		return false
	}
	for k, at := range t.pushRecent {
		if now.Sub(at) > pushCoalesce {
			delete(t.pushRecent, k)
		}
	}
	t.pushRecent[key] = now
	return true
}

func (t *Tenant) enqueuePushNotice(ctx context.Context, notice pushNotice) error {
	return t.enqueuePushPayload(ctx, pushPayload{Recipient: notice.recipient, Kind: notice.category, Text: notice.body, URL: notice.url, Actions: notice.actions})
}

// pushNotices decides who an event should wake and why.
func (t *Tenant) pushNotices(ctx context.Context, e event.Event) []pushNotice {
	base := strings.TrimRight(t.publicURL, "/")
	var notices []pushNotice
	// A request for a decision wakes the people asked in its own category
	// and nowhere else, so one event yields one notification per device.
	if kind, ok := approvalRequest(e); ok {
		return t.approvalNotices(e, kind)
	}
	switch e.Kind {
	case event.KIND_WRAP, 4:
		for _, recipient := range pushRecipients(e) {
			notices = append(notices, pushNotice{recipient: recipient, category: pushMessages, body: "New private message.", url: base + "/inbox"})
		}
	case event.KIND_GIT_ISSUE, event.KIND_GIT_PR:
		owner, identifier, ok := repoCoordinate(event.Tag(e, "a"))
		if !ok || owner == e.PubKey {
			return nil
		}
		label := "New issue"
		view := "issue"
		if e.Kind == event.KIND_GIT_PR {
			label, view = "New pull request", "pr"
		}
		body := label
		if subject := strings.TrimSpace(event.Tag(e, "subject")); subject != "" {
			body += ": " + excerpt(subject)
		}
		notices = append(notices, pushNotice{recipient: owner, category: pushReplies, body: body, url: base + "/repo?owner=" + owner + "&repo=" + identifier + "&view=" + view + "&id=" + e.ID})
	case 1, 1111, 30023:
		authored := t.referencedAuthors(ctx, e)
		for _, recipient := range pushRecipients(e) {
			category, body := pushMentions, "Mentioned you: "
			if authored[recipient] {
				category, body = pushReplies, "Replied to you: "
			}
			notices = append(notices, pushNotice{recipient: recipient, category: category, body: body + excerpt(e.Content), url: base + "/e/" + e.ID})
		}
	case event.KIND_CHAT, event.KIND_THREAD, event.KIND_THREAD_REPLY:
		// Room messages name the room so the summary reads as a conversation.
		roomID := event.Tag(e, "h")
		if roomID == "" || e.Kind == event.KIND_MARMOT_GROUP {
			return nil
		}
		room, err := t.community.Room(ctx, roomID)
		if err != nil || !room.Live() {
			return nil
		}
		name := room.Name
		if name == "" {
			name = room.ID
		}
		authored := t.referencedAuthors(ctx, e)
		for _, recipient := range pushRecipients(e) {
			category, body := pushMentions, "Mentioned you in "+name+": "
			if authored[recipient] {
				category, body = pushReplies, "Replied to you in "+name+": "
			}
			notices = append(notices, pushNotice{recipient: recipient, category: category, body: body + excerpt(e.Content), url: base + "/rooms/" + room.ID})
		}
	}
	return notices
}

// pushRecipients lists the addressed keys other than the author, bounded so
// a broadcast to a large list cannot fan out to every device.
func pushRecipients(e event.Event) []string {
	seen := map[string]struct{}{e.PubKey: {}}
	var recipients []string
	for _, tag := range e.Tags {
		if len(tag) < 2 || (tag[0] != "p" && tag[0] != "P") || len(tag[1]) != 64 {
			continue
		}
		if _, ok := seen[tag[1]]; ok {
			continue
		}
		seen[tag[1]] = struct{}{}
		recipients = append(recipients, tag[1])
		if len(recipients) == 16 {
			break
		}
	}
	return recipients
}

// referencedAuthors resolves the authors of events this one replies to, so a
// reply to your note counts as a reply rather than a mention.
func (t *Tenant) referencedAuthors(ctx context.Context, e event.Event) map[string]bool {
	authors := map[string]bool{}
	var ids []string
	for _, name := range []string{"e", "E"} {
		for _, id := range event.TagValues(e, name) {
			if len(id) == 64 && len(ids) < 8 {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return authors
	}
	rows, err := t.store.Query(ctx, event.Filter{IDs: ids, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: len(ids)})
	if err != nil {
		return authors
	}
	for _, row := range rows.Events {
		authors[row.PubKey] = true
	}
	return authors
}

func repoCoordinate(value string) (owner, identifier string, ok bool) {
	parts := strings.SplitN(value, ":", 3)
	if len(parts) != 3 || parts[0] != strconv.Itoa(30617) || len(parts[1]) != 64 || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func excerpt(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > pushExcerptMax {
		cut := pushExcerptMax
		for cut > 0 && cut < len(text) && text[cut]&0xC0 == 0x80 {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

// inboxSeen records when the person last opened their inbox, which bounds
// the unread count carried on the app badge.
func (t *Tenant) inboxSeen(ctx context.Context, pubkey string) int64 {
	var at int64
	_ = t.store.GetSetting(ctx, "inbox.seen."+pubkey, &at)
	return at
}

func (t *Tenant) markInboxSeen(ctx context.Context, pubkey string) error {
	return t.store.PutSetting(ctx, "inbox.seen."+pubkey, time.Now().Unix())
}

// inboxUnread counts events addressed to the key since it last opened the
// inbox, plus every request that still waits for the key's decision. It
// feeds the app badge; the page clears the conversation part on the next
// visit, and an answer clears the request.
func (t *Tenant) inboxUnread(ctx context.Context, pubkey string) int {
	since := t.inboxSeen(ctx, pubkey)
	// Count conversation kinds only, so relay records addressed to the
	// owner do not inflate the badge.
	filter := event.Filter{Kinds: []int{1, 4, event.KIND_CHAT, event.KIND_THREAD, event.KIND_THREAD_REPLY, 1111, event.KIND_WRAP, event.KIND_GIT_ISSUE, event.KIND_GIT_PR, 30023}, Tags: map[string][]string{"p": {pubkey}}}
	if since > 0 {
		filter.Since = &since
	}
	count, err := t.store.Count(ctx, []event.Filter{filter}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: []string{pubkey}}})
	if err != nil {
		return 0
	}
	// An open request counts once: those newer than the last visit are
	// already in the conversation count.
	for _, item := range t.openApprovals(ctx, pubkey) {
		if item.CreatedAt < since || !containsInt(filter.Kinds, item.Kind) {
			count++
		}
	}
	if count > 99 {
		return 99
	}
	return int(count)
}

func decodeCategories(raw string) map[string]bool {
	if raw == "" {
		return nil
	}
	var list []string
	if json.Unmarshal([]byte(raw), &list) != nil {
		return nil
	}
	allowed := map[string]bool{}
	for _, item := range list {
		allowed[item] = true
	}
	return allowed
}
