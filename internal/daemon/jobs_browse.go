package daemon

// Long task browsing: the job requests the caller may see, each settled
// against its answers, and one request with its answer timeline. Both read
// through the tenant gate with the caller's session, like the other browse
// methods.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gates"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// jobScan bounds how many stored requests or answers one listing walks.
const jobScan = 1000

// jobStates lists the state filters a listing accepts: one of the four
// settled states, or all.
var jobStates = []string{"open", "done", "failed", "cancelled", "all"}

// jobRequestKinds lists the kinds classified as long-task requests.
var jobRequestKinds = []int{event.KIND_JOB_REQUEST}

// jobAnswerKinds lists every kind that answers or ends a request.
var jobAnswerKinds = []int{event.KIND_JOB_ACCEPTED, event.KIND_JOB_PROGRESS, event.KIND_JOB_RESULT, event.KIND_JOB_CANCEL, event.KIND_JOB_ERROR}

type jobBrowseRequest struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
	State  string `json:"state"`
	Mine   bool   `json:"mine"`
	ID     string `json:"id"`
	Event  string `json:"event"`
}

// jobItem is one request as a browser sees it, settled against its answers.
type jobItem struct {
	ID          string      `json:"id"`
	Room        string      `json:"room"`
	Requester   string      `json:"requester"`
	Assignees   []string    `json:"assignees"`
	Subject     string      `json:"subject,omitempty"`
	Content     string      `json:"content"`
	CreatedAt   int64       `json:"created_at"`
	Expires     int64       `json:"expires,omitempty"`
	Root        string      `json:"root,omitempty"`
	State       string      `json:"state"`
	Status      string      `json:"status"`
	Progress    *jobAnswer  `json:"progress,omitempty"`
	Result      *jobAnswer  `json:"result,omitempty"`
	Error       *jobAnswer  `json:"error,omitempty"`
	CancelledAt int64       `json:"cancelled_at,omitempty"`
	Event       event.Event `json:"event"`
}

// jobAnswer is one accepted, progress, result, cancel or error event.
type jobAnswer struct {
	ID        string        `json:"id"`
	Kind      int           `json:"kind"`
	Provider  string        `json:"provider"`
	Content   string        `json:"content"`
	CreatedAt int64         `json:"created_at"`
	Artifacts []jobArtifact `json:"artifacts,omitempty"`
}

// jobArtifact is one artifact a result references: an event (e), an
// addressable event (a) or a URL (r).
type jobArtifact struct {
	Type  string `json:"type"`
	Value string `json:"value"`
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

// browseJobs lists the requests the caller may see, newest first, each
// settled against its answers. Expired requests are not served.
func (t *Tenant) browseJobs(ctx context.Context, actor string, q jobBrowseRequest) (any, error) {
	if q.State == "" {
		q.State = "all"
	}
	if !containsString(jobStates, q.State) {
		return nil, errors.New("invalid: job state must be open, done, failed, cancelled or all")
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

// browseJob returns one request with its answers, oldest first.
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
	timeline := make([]jobAnswer, 0, len(answers[item.ID]))
	for i := len(answers[item.ID]) - 1; i >= 0; i-- {
		timeline = append(timeline, jobAnswerFrom(answers[item.ID][i]))
	}
	return map[string]any{"item": item, "answers": timeline}, nil
}

// jobRoot names the thread a request attaches to: its e tag marked root.
func jobRoot(e event.Event) string {
	for _, tag := range e.Tags {
		if len(tag) > 3 && tag[0] == "e" && tag[3] == "root" && len(tag[1]) == 64 {
			return tag[1]
		}
	}
	return ""
}

func jobItemFrom(e event.Event) jobItem {
	assignees := event.TagValues(e, "p")
	if assignees == nil {
		assignees = []string{}
	}
	return jobItem{ID: e.ID, Room: event.Tag(e, "h"), Requester: e.PubKey, Assignees: assignees, Subject: strings.TrimSpace(event.Tag(e, "subject")), Content: e.Content, CreatedAt: e.CreatedAt, Expires: event.Expiration(e), Root: jobRoot(e), State: "open", Status: "queued", Event: e}
}

func jobAnswerFrom(row event.Event) jobAnswer {
	answer := jobAnswer{ID: row.ID, Kind: row.Kind, Provider: row.PubKey, Content: row.Content, CreatedAt: row.CreatedAt}
	if row.Kind != event.KIND_JOB_RESULT {
		return answer
	}
	request := false
	for _, tag := range row.Tags {
		if len(tag) < 2 || tag[1] == "" {
			continue
		}
		switch tag[0] {
		case "e":
			// The first e tag names the request; later ones are artifacts.
			if !request {
				request = true
				continue
			}
			answer.Artifacts = append(answer.Artifacts, jobArtifact{Type: "e", Value: tag[1]})
		case "a", "r":
			answer.Artifacts = append(answer.Artifacts, jobArtifact{Type: tag[0], Value: tag[1]})
		}
	}
	return answer
}

// jobAnswers finds the answers naming the given requests in one query,
// newest first, keeping only what the session may see.
func (t *Tenant) jobAnswers(ctx context.Context, items []jobItem, session relay.Session, now int64) (map[string][]event.Event, error) {
	answers := make(map[string][]event.Event, len(items))
	if len(items) == 0 {
		return answers, nil
	}
	ids := make([]string, 0, len(items))
	wanted := make(map[string]bool, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
		wanted[item.ID] = true
	}
	rows, err := t.store.Query(ctx, event.Filter{Kinds: jobAnswerKinds, Tags: map[string][]string{"e": ids}}, storage.QueryOptions{Now: now, Access: storage.Access{PubKeys: session.PubKeys}, Limit: jobScan})
	if err != nil {
		return nil, err
	}
	sortEventsNewestFirst(rows.Events)
	for _, row := range rows.Events {
		if !t.gate.CanSee(ctx, row, session, nil) {
			continue
		}
		if id := gates.JobRequestID(row); wanted[id] {
			answers[id] = append(answers[id], row)
		}
	}
	return answers, nil
}

// jobSettle derives one request's state from its answers, which arrive
// newest first. The newest terminal event wins: a result makes the task
// done, a cancel cancelled and an error failed. Otherwise the newest
// accepted or progress answer sets the status, and a request with no
// answers is queued.
func jobSettle(item *jobItem, rows []event.Event) {
	item.State, item.Status = "open", "queued"
	item.Progress, item.Result, item.Error, item.CancelledAt = nil, nil, nil, 0
	settled := false
	for _, row := range rows {
		answer := jobAnswerFrom(row)
		switch row.Kind {
		case event.KIND_JOB_RESULT:
			if item.Result == nil {
				item.Result = &answer
			}
			if !settled {
				item.State, item.Status, settled = "done", "done", true
			}
		case event.KIND_JOB_ERROR:
			if item.Error == nil {
				item.Error = &answer
			}
			if !settled {
				item.State, item.Status, settled = "failed", "failed", true
			}
		case event.KIND_JOB_CANCEL:
			if item.CancelledAt == 0 {
				item.CancelledAt = row.CreatedAt
			}
			if !settled {
				item.State, item.Status, settled = "cancelled", "cancelled", true
			}
		case event.KIND_JOB_ACCEPTED, event.KIND_JOB_PROGRESS:
			if item.Progress == nil {
				item.Progress = &answer
				if !settled {
					item.Status = "accepted"
					if row.Kind == event.KIND_JOB_PROGRESS {
						item.Status = "running"
					}
				}
			}
		}
	}
}

func jobCursor(item jobItem) string {
	return strconv.FormatInt(item.CreatedAt, 10) + ":" + item.ID
}
