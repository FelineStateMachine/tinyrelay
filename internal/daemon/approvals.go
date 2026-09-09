package daemon

// Approvals: a request for a decision is an ordinary comment or room message
// that carries a request tag and names the people asked with p tags. The
// asked person answers with a signed reaction (+ approves, - denies) or a
// reply. Nothing here introduces a kind; the asker learns the answer through
// the events it already watches.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const (
	kindComment     = 1111
	kindChatMessage = 9
	kindThreadRoot  = 11
	// approvalScan bounds how many stored events one listing walks.
	approvalScan = 1000
)

// approvalTypes is the bounded vocabulary of the request tag.
var approvalTypes = []string{"approve", "decide", "question"}
var approvalKinds = []int{kindComment, kindChatMessage, kindThreadRoot}

// approvalItem is one request as the asked person sees it.
type approvalItem struct {
	ID        string          `json:"id"`
	Kind      int             `json:"kind"`
	Type      string          `json:"type"`
	Asker     string          `json:"asker"`
	Asked     []string        `json:"asked"`
	Subject   string          `json:"subject,omitempty"`
	Content   string          `json:"content"`
	CreatedAt int64           `json:"created_at"`
	Expires   int64           `json:"expires,omitempty"`
	State     string          `json:"state"`
	Room      string          `json:"room,omitempty"`
	About     *approvalAbout  `json:"about,omitempty"`
	Answer    *approvalAnswer `json:"answer,omitempty"`
	Event     event.Event     `json:"event"`
}

// approvalAbout is what the request refers to: a repository coordinate, an
// event, or both, with the page that opens it.
type approvalAbout struct {
	Coordinate string `json:"coordinate,omitempty"`
	Event      string `json:"event,omitempty"`
	Kind       string `json:"kind,omitempty"`
	URL        string `json:"url"`
}

// approvalAnswer is the answer that counts: the newest reaction from an asked
// key, or the newest reply when nobody reacted.
type approvalAnswer struct {
	ID        string `json:"id"`
	Kind      int    `json:"kind"`
	Author    string `json:"author"`
	Decision  string `json:"decision"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
}

// approvalRequest reports whether the event asks anyone for a decision and
// which type it carries.
func approvalRequest(e event.Event) (string, bool) {
	if !containsInt(approvalKinds, e.Kind) {
		return "", false
	}
	kind := strings.ToLower(strings.TrimSpace(event.Tag(e, "request")))
	if !containsString(approvalTypes, kind) || len(approvalAsked(e)) == 0 {
		return "", false
	}
	return kind, true
}

// approvalAsked lists the keys asked: every p tag other than the asker.
func approvalAsked(e event.Event) []string {
	return pushRecipients(e)
}

func approvalItemFrom(e event.Event, kind string, base string) approvalItem {
	item := approvalItem{ID: e.ID, Kind: e.Kind, Type: kind, Asker: e.PubKey, Asked: approvalAsked(e), Subject: strings.TrimSpace(event.Tag(e, "subject")), Content: e.Content, CreatedAt: e.CreatedAt, Expires: event.Expiration(e), State: "open", Room: event.Tag(e, "h"), Event: e}
	item.About = approvalAboutFrom(e, base)
	return item
}

// approvalAboutFrom resolves the request's a or e reference to a page. A
// repository coordinate opens the repository, or its issue or pull request
// when the root is one; any other event opens the event page.
func approvalAboutFrom(e event.Event, base string) *approvalAbout {
	coordinate := ""
	for _, name := range []string{"a", "A"} {
		if value := event.Tag(e, name); value != "" {
			coordinate = value
			break
		}
	}
	referenced, kind := "", ""
	for _, ref := range [][2]string{{"E", "K"}, {"e", "k"}} {
		if value := event.Tag(e, ref[0]); len(value) == 64 {
			referenced, kind = value, event.Tag(e, ref[1])
			break
		}
	}
	if coordinate == "" && referenced == "" {
		return nil
	}
	about := &approvalAbout{Coordinate: coordinate, Event: referenced, Kind: kind}
	if owner, identifier, ok := repoCoordinate(coordinate); ok {
		about.URL = base + "/repo?owner=" + owner + "&repo=" + identifier + "&view=home"
		if view := map[string]string{"1621": "issue", "1618": "pr"}[kind]; view != "" && referenced != "" {
			about.URL = base + "/repo?owner=" + owner + "&repo=" + identifier + "&view=" + view + "&id=" + referenced
		}
	} else if referenced != "" {
		about.URL = base + "/e/" + referenced
	} else {
		about.URL = base + "/a/" + coordinate
	}
	return about
}

// approvalDecision maps a reaction to the answer it records. An empty
// reaction counts as approval, as NIP-25 reads it.
func approvalDecision(content string) string {
	switch strings.TrimSpace(content) {
	case "+", "":
		return "approved"
	case "-":
		return "denied"
	}
	return ""
}

// approvalAnswers finds the answers to the given requests in one query and
// settles each request's state. The newest reaction from an asked key wins;
// a reply from an asked key answers a request that has no reaction.
func (t *Tenant) approvalAnswers(ctx context.Context, items []approvalItem, now int64) (map[string][]event.Event, error) {
	answers := make(map[string][]event.Event, len(items))
	if len(items) == 0 {
		return answers, nil
	}
	ids := make([]string, 0, len(items))
	authors := []string{}
	asked := make(map[string]map[string]bool, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
		authors = append(authors, item.Asked...)
		asked[item.ID] = map[string]bool{}
		for _, key := range item.Asked {
			asked[item.ID][key] = true
		}
	}
	rows, err := t.store.Query(ctx, event.Filter{Kinds: []int{kindReaction, kindComment}, Authors: uniqueStrings(authors), Tags: map[string][]string{"e": ids}}, storage.QueryOptions{Now: now, Access: storage.Access{All: true}, Limit: approvalScan})
	if err != nil {
		return nil, err
	}
	sortEventsNewestFirst(rows.Events)
	for _, row := range rows.Events {
		if _, isRequest := approvalRequest(row); isRequest {
			continue
		}
		for _, id := range event.TagValues(row, "e") {
			if asked[id] != nil && asked[id][row.PubKey] {
				answers[id] = append(answers[id], row)
			}
		}
	}
	return answers, nil
}

// approvalSettle fills the answer and state of one request from its answers,
// which arrive newest first.
func approvalSettle(item *approvalItem, rows []event.Event, now int64) {
	var reply *event.Event
	for i := range rows {
		row := rows[i]
		if row.Kind == kindReaction {
			if decision := approvalDecision(row.Content); decision != "" {
				item.Answer = &approvalAnswer{ID: row.ID, Kind: row.Kind, Author: row.PubKey, Decision: decision, Content: row.Content, CreatedAt: row.CreatedAt}
				item.State = "answered"
				return
			}
		} else if reply == nil {
			reply = &row
		}
	}
	if reply != nil {
		item.Answer = &approvalAnswer{ID: reply.ID, Kind: reply.Kind, Author: reply.PubKey, Decision: "replied", Content: reply.Content, CreatedAt: reply.CreatedAt}
		item.State = "answered"
		return
	}
	if item.Expires > 0 && item.Expires <= now {
		item.State = "expired"
	}
}

// approvalsFor lists every request addressed to the key, newest first,
// settled against its answers. Expired requests are kept so the person sees
// what lapsed until maintenance removes them.
func (t *Tenant) approvalsFor(ctx context.Context, pubkey string, before *storage.EventCursor, limit int) ([]approvalItem, bool, error) {
	now := time.Now().Unix()
	base := strings.TrimRight(t.publicURL, "/")
	page, err := t.store.Query(ctx, event.Filter{Kinds: approvalKinds, Tags: map[string][]string{"p": {pubkey}}}, storage.QueryOptions{Access: storage.Access{PubKeys: []string{pubkey}}, Limit: limit, Before: before})
	if err != nil {
		return nil, false, err
	}
	items := make([]approvalItem, 0, len(page.Events))
	for _, row := range page.Events {
		kind, ok := approvalRequest(row)
		if !ok || row.PubKey == pubkey {
			continue
		}
		items = append(items, approvalItemFrom(row, kind, base))
	}
	answers, err := t.approvalAnswers(ctx, items, now)
	if err != nil {
		return nil, false, err
	}
	for i := range items {
		approvalSettle(&items[i], answers[items[i].ID], now)
	}
	return items, page.More, nil
}

// openApprovals lists the unanswered, unexpired requests addressed to the
// key, which the badge and the panel count.
func (t *Tenant) openApprovals(ctx context.Context, pubkey string) []approvalItem {
	var open []approvalItem
	var cursor *storage.EventCursor
	for scanned := 0; scanned < approvalScan; scanned += 100 {
		items, more, err := t.approvalsFor(ctx, pubkey, cursor, 100)
		if err != nil {
			return open
		}
		for _, item := range items {
			if item.State == "open" {
				open = append(open, item)
			}
		}
		if !more || len(items) == 0 {
			break
		}
		last := items[len(items)-1]
		cursor = &storage.EventCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return open
}

// approvalNotices wakes each asked person in the approvals category. The
// body names the asker and the subject, and the payload carries the actions
// the device can answer with: approve and deny for decisions, reply for
// every type.
func (t *Tenant) approvalNotices(e event.Event, kind string) []pushNotice {
	base := strings.TrimRight(t.publicURL, "/")
	subject := strings.TrimSpace(event.Tag(e, "subject"))
	if subject == "" {
		subject = e.Content
	}
	body := shortKey(e.PubKey) + " asks: " + excerpt(subject)
	actions := []pushAction{{Action: "reply", Title: "Reply"}}
	if kind != "question" {
		actions = []pushAction{{Action: "approve", Title: "Approve"}, {Action: "deny", Title: "Deny"}, {Action: "reply", Title: "Reply"}}
	}
	var notices []pushNotice
	for _, recipient := range approvalAsked(e) {
		notices = append(notices, pushNotice{recipient: recipient, category: pushApprovals, body: body, url: base + "/approvals?id=" + e.ID, actions: actions})
	}
	return notices
}

func shortKey(pubkey string) string {
	if len(pubkey) > 12 {
		return pubkey[:12]
	}
	return pubkey
}

func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func approvalCursor(item approvalItem) string {
	return strconv.FormatInt(item.CreatedAt, 10) + ":" + item.ID
}
