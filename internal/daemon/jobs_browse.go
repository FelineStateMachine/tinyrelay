package daemon

// Long task browsing: the NIP-90 job requests the caller may see, each with
// its newest feedback status and its result when one exists, and one
// request with its feedback timeline and results. Both read through the
// tenant gate with the caller's session, like the other browse methods.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// jobScan bounds how many stored requests or answers one listing walks.
const jobScan = 1000

var jobStates = []string{"open", "done", "all"}

// jobRequestKinds lists the kinds classified as long-task requests.
var jobRequestKinds = func() []int {
	kinds := make([]int, 0, event.KIND_JOB_REQUEST_MAX-event.KIND_JOB_REQUEST_MIN+1)
	for kind := event.KIND_JOB_REQUEST_MIN; kind <= event.KIND_JOB_REQUEST_MAX; kind++ {
		if event.IsJobRequest(kind) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}()

type jobBrowseRequest struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
	State  string `json:"state"`
	Mine   bool   `json:"mine"`
	ID     string `json:"id"`
	Event  string `json:"event"`
}

// jobInput is one i tag of a request.
type jobInput struct {
	Data   string `json:"data"`
	Type   string `json:"type"`
	Relay  string `json:"relay,omitempty"`
	Marker string `json:"marker,omitempty"`
}

// jobParam is one param tag of a request.
type jobParam struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// jobItem is one request as a browser sees it, settled against its answers.
type jobItem struct {
	ID        string      `json:"id"`
	Kind      int         `json:"kind"`
	Requester string      `json:"requester"`
	Providers []string    `json:"providers"`
	Inputs    []jobInput  `json:"inputs"`
	Output    string      `json:"output,omitempty"`
	Params    []jobParam  `json:"params"`
	Bid       string      `json:"bid,omitempty"`
	Relays    []string    `json:"relays"`
	Encrypted bool        `json:"encrypted,omitempty"`
	Content   string      `json:"content"`
	CreatedAt int64       `json:"created_at"`
	Expires   int64       `json:"expires,omitempty"`
	State     string      `json:"state"`
	Status    string      `json:"status,omitempty"`
	Feedback  *jobAnswer  `json:"feedback,omitempty"`
	Result    *jobAnswer  `json:"result,omitempty"`
	Event     event.Event `json:"event"`
}

// jobAnswer is one feedback or result event from a service provider.
type jobAnswer struct {
	ID        string `json:"id"`
	Kind      int    `json:"kind"`
	Provider  string `json:"provider"`
	Status    string `json:"status,omitempty"`
	Info      string `json:"info,omitempty"`
	Amount    string `json:"amount,omitempty"`
	Invoice   string `json:"invoice,omitempty"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
}

func jobBrowseMethod(method string) bool {
	return method == "browsejobs" || method == "browsejob"
}

func (t *Tenant) executeJobs(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
	if !t.Policy().Features.Jobs {
		return nil, errors.New("restricted: long tasks are switched off on this relay")
	}
	q := jobBrowseRequest{}
	if len(params) > 0 {
		if err := json.Unmarshal(params[0], &q); err != nil {
			return nil, fmt.Errorf("invalid: job parameters: %w", err)
		}
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 50
	}
	switch method {
	case "browsejobs":
		return t.browseJobs(ctx, actor, q)
	case "browsejob":
		if q.ID == "" {
			q.ID = q.Event
		}
		return t.browseJob(ctx, actor, q.ID)
	}
	return nil, errors.New("unsupported: browse operation")
}

// browseJobs lists the requests the caller may see, newest first. A request
// is done once a result exists or its newest feedback reports success or an
// error; every other request is open. Expired requests are not served.
func (t *Tenant) browseJobs(ctx context.Context, actor string, q jobBrowseRequest) (any, error) {
	if q.State == "" {
		q.State = "all"
	}
	if !containsString(jobStates, q.State) {
		return nil, errors.New("invalid: job state must be open, done or all")
	}
	if q.Mine && actor == "" {
		return nil, errors.New("auth-required: sign in to list your own requests")
	}
	cursor, err := parseCollaborationCursor(q.Cursor)
	if err != nil {
		return nil, errors.New("invalid: job cursor")
	}
	session := browseSession(t, actor)
	now := time.Now().Unix()
	filter := event.Filter{Kinds: jobRequestKinds, Tags: map[string][]string{}}
	if q.Mine {
		filter.Authors = []string{actor}
	}
	items := make([]jobItem, 0, q.Limit)
	next := ""
	for scanned := 0; scanned < jobScan && next == ""; scanned += 100 {
		page, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: now, Access: storage.Access{PubKeys: session.PubKeys}, Limit: 100, Before: cursor})
		if err != nil {
			return nil, err
		}
		visible := make([]jobItem, 0, len(page.Events))
		for _, row := range page.Events {
			cursor = &storage.EventCursor{CreatedAt: row.CreatedAt, ID: row.ID}
			if t.gate.CanSee(ctx, row, session, nil) {
				visible = append(visible, jobItemFrom(row))
			}
		}
		answers, err := t.jobAnswers(ctx, visible, session, now)
		if err != nil {
			return nil, err
		}
		for i := range visible {
			jobSettle(&visible[i], answers[visible[i].ID])
			if q.State != "all" && visible[i].State != q.State {
				continue
			}
			if len(items) == q.Limit {
				next = jobCursor(items[len(items)-1])
				break
			}
			items = append(items, visible[i])
		}
		if !page.More || len(page.Events) == 0 {
			break
		}
		if next == "" && len(items) == q.Limit {
			next = jobCursor(items[len(items)-1])
		}
	}
	return map[string]any{"items": items, "next_cursor": next}, nil
}

// browseJob returns one request with its feedback, oldest first, and its
// results, newest first.
func (t *Tenant) browseJob(ctx context.Context, actor, id string) (any, error) {
	if len(id) != 64 {
		return nil, errors.New("invalid: job id")
	}
	session := browseSession(t, actor)
	rows, err := t.Query(ctx, []event.Filter{{IDs: []string{id}, Kinds: jobRequestKinds, Limit: intPtr(1)}}, session)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, errors.New("not found: job request")
	}
	item := jobItemFrom(rows[0])
	answers, err := t.jobAnswers(ctx, []jobItem{item}, session, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	jobSettle(&item, answers[item.ID])
	feedback, results := []jobAnswer{}, []jobAnswer{}
	for _, row := range answers[item.ID] {
		if row.Kind == event.KIND_JOB_FEEDBACK {
			feedback = append([]jobAnswer{jobAnswerFrom(row)}, feedback...)
		} else {
			results = append(results, jobAnswerFrom(row))
		}
	}
	return map[string]any{"item": item, "feedback": feedback, "results": results}, nil
}

func jobItemFrom(e event.Event) jobItem {
	item := jobItem{ID: e.ID, Kind: e.Kind, Requester: e.PubKey, Providers: event.TagValues(e, "p"), Inputs: []jobInput{}, Params: []jobParam{}, Relays: []string{}, Output: event.Tag(e, "output"), Bid: event.Tag(e, "bid"), Content: e.Content, CreatedAt: e.CreatedAt, Expires: event.Expiration(e), State: "open", Event: e}
	for _, tag := range e.Tags {
		switch {
		case len(tag) >= 3 && tag[0] == "i":
			input := jobInput{Data: tag[1], Type: tag[2]}
			if len(tag) > 3 {
				input.Relay = tag[3]
			}
			if len(tag) > 4 {
				input.Marker = tag[4]
			}
			item.Inputs = append(item.Inputs, input)
		case len(tag) >= 3 && tag[0] == "param":
			item.Params = append(item.Params, jobParam{Key: tag[1], Value: tag[2]})
		case len(tag) >= 2 && tag[0] == "relays":
			item.Relays = append(item.Relays, tag[1:]...)
		case len(tag) >= 1 && tag[0] == "encrypted":
			item.Encrypted = true
		}
	}
	return item
}

func jobAnswerFrom(row event.Event) jobAnswer {
	answer := jobAnswer{ID: row.ID, Kind: row.Kind, Provider: row.PubKey, Content: row.Content, CreatedAt: row.CreatedAt}
	for _, tag := range row.Tags {
		switch {
		case len(tag) >= 2 && tag[0] == "status" && answer.Status == "":
			answer.Status = tag[1]
			if len(tag) > 2 {
				answer.Info = tag[2]
			}
		case len(tag) >= 2 && tag[0] == "amount" && answer.Amount == "":
			answer.Amount = tag[1]
			if len(tag) > 2 {
				answer.Invoice = tag[2]
			}
		}
	}
	return answer
}

// jobAnswers finds the feedback and results naming the given requests in
// one query, newest first, keeping only what the session may see.
func (t *Tenant) jobAnswers(ctx context.Context, items []jobItem, session relay.Session, now int64) (map[string][]event.Event, error) {
	answers := make(map[string][]event.Event, len(items))
	if len(items) == 0 {
		return answers, nil
	}
	ids := make([]string, 0, len(items))
	kinds := []int{event.KIND_JOB_FEEDBACK}
	wanted := make(map[string]int, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
		wanted[item.ID] = event.JobResultKind(item.Kind)
		if !containsInt(kinds, wanted[item.ID]) {
			kinds = append(kinds, wanted[item.ID])
		}
	}
	rows, err := t.store.Query(ctx, event.Filter{Kinds: kinds, Tags: map[string][]string{"e": ids}}, storage.QueryOptions{Now: now, Access: storage.Access{PubKeys: session.PubKeys}, Limit: jobScan})
	if err != nil {
		return nil, err
	}
	sortEventsNewestFirst(rows.Events)
	for _, row := range rows.Events {
		if !t.gate.CanSee(ctx, row, session, nil) {
			continue
		}
		for _, id := range event.TagValues(row, "e") {
			resultKind, ok := wanted[id]
			if ok && (row.Kind == event.KIND_JOB_FEEDBACK || row.Kind == resultKind) {
				answers[id] = append(answers[id], row)
			}
		}
	}
	return answers, nil
}

// jobSettle fills the newest feedback, the newest result and the state of
// one request from its answers, which arrive newest first.
func jobSettle(item *jobItem, rows []event.Event) {
	for _, row := range rows {
		answer := jobAnswerFrom(row)
		if row.Kind == event.KIND_JOB_FEEDBACK {
			if item.Feedback == nil {
				item.Feedback = &answer
				item.Status = answer.Status
			}
		} else if item.Result == nil {
			item.Result = &answer
		}
	}
	item.State = "open"
	if item.Result != nil || item.Status == "success" || item.Status == "error" {
		item.State = "done"
	}
}

func jobCursor(item jobItem) string {
	return strconv.FormatInt(item.CreatedAt, 10) + ":" + item.ID
}
