package daemon

// Custom views hand fenced code blocks to an external transform and keep
// what comes back as relay-signed artifacts attached to the source event.
// This file holds the definition, its storage and the owner's management
// methods; custom_views_transform.go holds the block extraction, the POST
// and the artifacts, and custom_views_http.go serves them.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/views"
)

const (
	viewTransform        = "view-transform"
	viewKindMax          = 16
	viewLanguageMax      = 32
	viewLanguageLength   = 32
	viewDefaultMaxBytes  = 1 << 20
	viewMaxBytesCap      = 4 << 20
	viewTimeout          = 30 * time.Second
	viewMaxAttempts      = 3
	viewPauseFailures    = 20
	viewBackfillLimit    = 500
	viewHourlyPeriod     = int64(3600)
	viewResponseMaxBytes = int64(viewMaxBytesCap*4 + 1<<20)
)

// viewBackoff is the wait before the second and third attempt.
var viewBackoff = []time.Duration{time.Minute, 5 * time.Minute}

const customViewSchema = `CREATE TABLE IF NOT EXISTS custom_views(name TEXT PRIMARY KEY, kinds TEXT NOT NULL, transform TEXT NOT NULL, trigger TEXT NOT NULL, audience TEXT NOT NULL, languages TEXT NOT NULL, max_bytes INTEGER NOT NULL, secret TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, last_run_at INTEGER NOT NULL DEFAULT 0, last_status TEXT NOT NULL DEFAULT '', failures INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS custom_view_artifacts(view TEXT NOT NULL, hash TEXT NOT NULL, event_id TEXT NOT NULL, type TEXT NOT NULL, engine TEXT NOT NULL DEFAULT '', audience TEXT NOT NULL, content TEXT NOT NULL, raw TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY(view,hash));
CREATE TABLE IF NOT EXISTS custom_view_sources(view TEXT NOT NULL, hash TEXT NOT NULL, source TEXT NOT NULL, block INTEGER NOT NULL, expires INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(view,hash,source));
CREATE INDEX IF NOT EXISTS custom_view_sources_source ON custom_view_sources(view,source);`

// customView is one stored definition. The secret never leaves the tenant
// except once, in the addcustomview answer.
type customView struct {
	Name       string   `json:"name"`
	Kinds      []int    `json:"kinds"`
	Transform  string   `json:"transform"`
	Trigger    string   `json:"trigger"`
	Audience   string   `json:"audience"`
	Languages  []string `json:"languages"`
	MaxBytes   int      `json:"maxBytes"`
	Enabled    bool     `json:"enabled"`
	CreatedAt  int64    `json:"created"`
	LastRunAt  int64    `json:"lastRun"`
	LastStatus string   `json:"lastStatus"`
	Failures   int      `json:"failures"`
	secret     string
}

func (v customView) summary() map[string]any {
	state := "active"
	if !v.Enabled {
		state = "paused"
	}
	return map[string]any{"name": v.Name, "kinds": v.Kinds, "transform": v.Transform, "host": callbackHost(v.Transform), "trigger": v.Trigger, "audience": v.Audience, "languages": v.Languages, "maxBytes": v.MaxBytes, "enabled": v.Enabled, "state": state, "created": v.CreatedAt, "lastRun": v.LastRunAt, "lastStatus": v.LastStatus, "failures": v.Failures}
}

func (v customView) watches(kind int) bool {
	for _, k := range v.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func (t *Tenant) initCustomViews(ctx context.Context) error {
	_, err := t.store.DB().ExecContext(ctx, customViewSchema)
	return err
}

func customViewMethod(method string) bool {
	return containsString([]string{"listcustomviews", "addcustomview", "removecustomview", "runcustomview", "pausecustomview", "resumecustomview"}, method)
}

// customViewExecute serves the owner's custom view methods under the view
// operation. Every change is recorded in the audit log.
func (t *Tenant) customViewExecute(ctx context.Context, actor, method string, params []json.RawMessage) (result any, err error) {
	ctx, finish := t.app.telemetry.Start(ctx, "view")
	outcome := "error"
	defer func() {
		if err != nil {
			outcome = callbackOutcome(err)
		}
		finish(outcome)
	}()
	role, err := t.community.Role(ctx, actor)
	if err != nil {
		return nil, err
	}
	if role != "owner" {
		return nil, fmt.Errorf("restricted: only the owner may %s", method)
	}
	switch method {
	case "listcustomviews":
		rows, err := t.customViewRows(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.summary())
		}
		outcome = "ok"
		return out, nil
	case "addcustomview":
		result, err = t.addCustomView(ctx, actor, params)
	default:
		result, err = t.changeCustomView(ctx, actor, method, params)
	}
	if err == nil {
		outcome = "ok"
	}
	return result, err
}

// customViewOptions is the addcustomview parameter. Kinds and languages may
// arrive as lists or as comma-separated strings from a plain form.
type customViewOptions struct {
	Name      string          `json:"name"`
	Kinds     json.RawMessage `json:"kinds"`
	Transform string          `json:"transform"`
	Trigger   string          `json:"trigger"`
	Audience  string          `json:"audience"`
	Languages json.RawMessage `json:"languages"`
	MaxBytes  json.RawMessage `json:"max_bytes"`
	Secret    string          `json:"secret"`
}

func (t *Tenant) addCustomView(ctx context.Context, actor string, params []json.RawMessage) (any, error) {
	if len(params) != 1 {
		return nil, errors.New("invalid: addcustomview expects one object with name, kinds, transform, trigger, audience, languages and optional max_bytes and secret")
	}
	var options customViewOptions
	if err := json.Unmarshal(params[0], &options); err != nil {
		return nil, errors.New("invalid: addcustomview expects one object with name, kinds, transform, trigger, audience, languages and optional max_bytes and secret")
	}
	view, err := t.parseCustomView(options)
	if err != nil {
		return nil, err
	}
	generated := false
	secret := strings.TrimSpace(options.Secret)
	if secret == "" {
		secret, err = randomHex(32)
		if err != nil {
			return nil, err
		}
		generated = true
	} else if len(secret) < callbackSecretMin || len(secret) > callbackSecretMax || strings.Trim(secret, "!\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~") != "" {
		return nil, fmt.Errorf("invalid: secret must be %d to %d printable ASCII characters", callbackSecretMin, callbackSecretMax)
	}
	view.secret = secret
	view.Enabled = true
	view.CreatedAt = time.Now().Unix()
	kinds, _ := json.Marshal(view.Kinds)
	languages, _ := json.Marshal(view.Languages)
	_, err = t.store.DB().ExecContext(ctx, `INSERT INTO custom_views(name,kinds,transform,trigger,audience,languages,max_bytes,secret,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,1,?)`, view.Name, string(kinds), view.Transform, view.Trigger, view.Audience, string(languages), view.MaxBytes, secret, view.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errors.New("invalid: a view with that name exists")
		}
		return nil, err
	}
	t.invalidateCustomViews()
	if err := t.community.Record(ctx, actor, "addcustomview", view.Name, ""); err != nil {
		return nil, err
	}
	// Counts only: the transform host, path and secret stay out of the log.
	t.app.telemetry.Logger().Info("custom view added", "tenant", t.meta.Name, "kinds", len(view.Kinds), "languages", len(view.Languages), "generated", generated)
	out := view.summary()
	out["secret"] = secret
	return out, nil
}

// parseCustomView validates a definition: the name, the kinds it watches,
// the https transform, the trigger, the audience, the languages and the
// artifact size limit.
func (t *Tenant) parseCustomView(options customViewOptions) (customView, error) {
	view := customView{Name: strings.TrimSpace(options.Name), Transform: strings.TrimSpace(options.Transform), Trigger: strings.TrimSpace(options.Trigger), Audience: strings.TrimSpace(options.Audience), MaxBytes: viewDefaultMaxBytes}
	if !views.NamePattern.MatchString(view.Name) {
		return customView{}, errors.New("invalid: name must be 1 to 32 lowercase letters, digits or hyphens")
	}
	kinds, err := intList(options.Kinds)
	if err != nil || len(kinds) == 0 || len(kinds) > viewKindMax {
		return customView{}, fmt.Errorf("invalid: kinds must list 1 to %d event kinds", viewKindMax)
	}
	for _, kind := range kinds {
		if kind < 0 || kind > 65535 {
			return customView{}, errors.New("invalid: kinds must be between 0 and 65535")
		}
	}
	sort.Ints(kinds)
	view.Kinds = dedupeInts(kinds)
	if err := t.checkCallbackURL(view.Transform); err != nil {
		return customView{}, fmt.Errorf("invalid: transform %s", strings.TrimPrefix(err.Error(), "invalid: "))
	}
	if view.Trigger == "" {
		view.Trigger = "write"
	}
	if view.Trigger != "write" && view.Trigger != "hourly" {
		return customView{}, errors.New("invalid: trigger must be write or hourly")
	}
	if view.Audience == "" {
		view.Audience = "public"
	}
	if view.Audience != "public" && view.Audience != "members" {
		return customView{}, errors.New("invalid: audience must be public or members")
	}
	languages, err := stringList(options.Languages)
	if err != nil || len(languages) == 0 || len(languages) > viewLanguageMax {
		return customView{}, fmt.Errorf("invalid: languages must list 1 to %d fenced block languages", viewLanguageMax)
	}
	seen := map[string]bool{}
	view.Languages = nil
	for _, lang := range languages {
		lang = strings.ToLower(strings.TrimSpace(lang))
		if lang == "" || len(lang) > viewLanguageLength || strings.Trim(lang, "abcdefghijklmnopqrstuvwxyz0123456789+-_.#") != "" {
			return customView{}, errors.New("invalid: languages must be lowercase words of letters, digits, +, -, _, . or #")
		}
		if !seen[lang] {
			seen[lang] = true
			view.Languages = append(view.Languages, lang)
		}
	}
	if len(options.MaxBytes) > 0 && string(options.MaxBytes) != "null" && string(options.MaxBytes) != `""` {
		var size int
		var text string
		if json.Unmarshal(options.MaxBytes, &size) != nil {
			if json.Unmarshal(options.MaxBytes, &text) != nil {
				return customView{}, errors.New("invalid: max_bytes must be a number")
			}
			if size, err = strconv.Atoi(strings.TrimSpace(text)); err != nil {
				return customView{}, errors.New("invalid: max_bytes must be a number")
			}
		}
		if size < 1 || size > viewMaxBytesCap {
			return customView{}, fmt.Errorf("invalid: max_bytes must be between 1 and %d", viewMaxBytesCap)
		}
		view.MaxBytes = size
	}
	return view, nil
}

// intList reads a JSON list of integers, a list of numeric strings or a
// comma-separated string.
func intList(raw json.RawMessage) ([]int, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []int
	if json.Unmarshal(raw, &out) == nil {
		return out, nil
	}
	items, err := stringList(raw)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		value, err := strconv.Atoi(strings.TrimSpace(item))
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// stringList reads a JSON list of strings or a comma-separated string.
func stringList(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []string
	if json.Unmarshal(raw, &out) == nil {
		return out, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, err
	}
	for _, item := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out, nil
}

func dedupeInts(values []int) []int {
	out := values[:0]
	for i, value := range values {
		if i == 0 || value != values[i-1] {
			out = append(out, value)
		}
	}
	return out
}

func (t *Tenant) changeCustomView(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
	name := customViewNameParam(params)
	if name == "" {
		return nil, errors.New("invalid: view name required")
	}
	view, err := t.customViewByName(ctx, name)
	if err != nil {
		return nil, err
	}
	switch method {
	case "removecustomview":
		if err := t.removeCustomView(ctx, view); err != nil {
			return nil, err
		}
	case "pausecustomview":
		_, err = t.store.DB().ExecContext(ctx, `UPDATE custom_views SET enabled=0, last_status='paused' WHERE name=?`, name)
		view.Enabled, view.LastStatus = false, "paused"
	case "resumecustomview":
		_, err = t.store.DB().ExecContext(ctx, `UPDATE custom_views SET enabled=1, failures=0, last_status='' WHERE name=?`, name)
		view.Enabled, view.Failures, view.LastStatus = true, 0, ""
	case "runcustomview":
		if !view.Enabled {
			return nil, errors.New("invalid: resume the view before running it")
		}
		queued, err := t.queueCustomViewBackfill(ctx, view, 0, strconv.FormatInt(time.Now().UnixNano(), 10))
		if err != nil {
			return nil, err
		}
		if err := t.community.Record(ctx, actor, method, name, strconv.Itoa(queued)); err != nil {
			return nil, err
		}
		t.app.telemetry.Logger().Info("custom view run queued", "tenant", t.meta.Name, "queued", queued)
		out := view.summary()
		out["queued"] = queued
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported: management method %q", method)
	}
	if err != nil {
		return nil, err
	}
	t.invalidateCustomViews()
	if err := t.community.Record(ctx, actor, method, name, ""); err != nil {
		return nil, err
	}
	if method == "removecustomview" {
		return map[string]any{"name": name, "removed": true}, nil
	}
	return view.summary(), nil
}

// removeCustomView deletes the definition and every artifact it produced,
// including the stored public records.
func (t *Tenant) removeCustomView(ctx context.Context, view customView) error {
	ids, err := t.customViewArtifactIDs(ctx, `SELECT event_id FROM custom_view_artifacts WHERE view=?`, view.Name)
	if err != nil {
		return err
	}
	if err := t.store.WithTx(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"custom_view_sources", "custom_view_artifacts", "custom_views"} {
			column := "view"
			if table == "custom_views" {
				column = "name"
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+column+"=?", view.Name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return t.deleteStoredArtifacts(ctx, ids)
}

// customViewNameParam reads the name from a bare string or from {"name": ...}.
func customViewNameParam(params []json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	if name := stringParam(params, 0); name != "" {
		return name
	}
	var options struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(params[0], &options)
	return strings.TrimSpace(options.Name)
}

func (t *Tenant) customViewByName(ctx context.Context, name string) (customView, error) {
	rows, err := t.customViewRows(ctx, name)
	if err != nil {
		return customView{}, err
	}
	if len(rows) == 0 {
		return customView{}, errors.New("not found: view")
	}
	return rows[0], nil
}

// customViewRows lists definitions, narrowed to one name.
func (t *Tenant) customViewRows(ctx context.Context, names ...string) ([]customView, error) {
	query := `SELECT name,kinds,transform,trigger,audience,languages,max_bytes,secret,enabled,created_at,last_run_at,last_status,failures FROM custom_views`
	args := []any{}
	if len(names) > 0 {
		query += ` WHERE name=?`
		args = append(args, names[0])
	}
	query += ` ORDER BY created_at,rowid`
	rows, err := t.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list custom views: %w", err)
	}
	defer rows.Close()
	var out []customView
	for rows.Next() {
		var item customView
		var kinds, languages string
		var enabled int
		if err := rows.Scan(&item.Name, &kinds, &item.Transform, &item.Trigger, &item.Audience, &languages, &item.MaxBytes, &item.secret, &enabled, &item.CreatedAt, &item.LastRunAt, &item.LastStatus, &item.Failures); err != nil {
			return nil, err
		}
		item.Enabled = enabled != 0
		if err := json.Unmarshal([]byte(kinds), &item.Kinds); err != nil {
			return nil, fmt.Errorf("custom view kinds: %w", err)
		}
		if err := json.Unmarshal([]byte(languages), &item.Languages); err != nil {
			return nil, fmt.Errorf("custom view languages: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// customViewIndex returns the enabled views indexed by kind, built on first
// use and dropped whenever a view changes.
func (t *Tenant) customViewIndex(ctx context.Context) map[int][]customView {
	t.viewMu.Lock()
	defer t.viewMu.Unlock()
	if t.viewIndex != nil {
		return t.viewIndex
	}
	index := map[int][]customView{}
	rows, err := t.customViewRows(ctx)
	if err != nil {
		t.app.telemetry.Logger().Error("load custom views", "tenant", t.meta.Name, "error", err)
		return index
	}
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		for _, kind := range row.Kinds {
			index[kind] = append(index[kind], row)
		}
	}
	t.viewIndex = index
	return index
}

func (t *Tenant) invalidateCustomViews() {
	t.viewMu.Lock()
	t.viewIndex = nil
	t.viewMu.Unlock()
}

// CustomViews is the summary the web UI renders with: the enabled views'
// names and languages, nothing else.
func (t *Tenant) CustomViews() []views.View {
	seen := map[string]bool{}
	var out []views.View
	for _, list := range t.customViewIndex(context.Background()) {
		for _, view := range list {
			if !seen[view.Name] {
				seen[view.Name] = true
				out = append(out, views.View{Name: view.Name, Languages: append([]string(nil), view.Languages...)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// queueCustomViews runs after an event is stored or released. Each enabled
// write-triggered view that watches the kind and finds a block it renders
// gets one transform intent. It never fails the publish.
func (t *Tenant) queueCustomViews(ctx context.Context, e event.Event) {
	candidates := t.customViewIndex(ctx)[e.Kind]
	if len(candidates) == 0 {
		return
	}
	if e.Kind == event.KIND_REPO_STATE && (t.git == nil || t.statePending(ctx, e.ID)) {
		// The README is read from the objects, which have not arrived yet;
		// releaseGit queues the state once they have.
		return
	}
	var intents []storage.Intent
	for _, view := range candidates {
		if view.Trigger != "write" {
			continue
		}
		if e.Kind != event.KIND_REPO_STATE && len(views.Matching(views.Blocks(e.Content), view.Languages)) == 0 {
			continue
		}
		intents = append(intents, t.viewIntent(view, e, ""))
	}
	if len(intents) == 0 {
		return
	}
	err := t.store.WithTx(ctx, func(tx *sql.Tx) error {
		return storage.AddIntents(ctx, tx, intents, time.Now().Unix())
	})
	if err != nil {
		t.app.telemetry.Logger().Error("view transforms not queued", "tenant", t.meta.Name, "error", err)
		return
	}
	t.app.telemetry.Logger().Debug("view transforms queued", "tenant", t.meta.Name, "queued", len(intents))
}

// viewIntent is the transform intent for one source. The event travels in
// the payload; run makes a backfill or an hourly pass distinct from the
// write-time intent for the same event.
func (t *Tenant) viewIntent(view customView, e event.Event, run string) storage.Intent {
	payload, _ := json.Marshal(viewPayload{Event: e, Attempt: 1, Run: run})
	id := e.ID
	if run != "" {
		id += "@" + run
	}
	return storage.Intent{Kind: viewTransform, EventID: id, Target: view.Name, Payload: string(payload)}
}

// statePending reports whether a repository state still waits for its
// objects.
func (t *Tenant) statePending(ctx context.Context, id string) bool {
	var one int
	err := t.store.DB().QueryRowContext(ctx, `SELECT 1 FROM pending_events WHERE id=?`, id).Scan(&one)
	return err == nil || t.git.IsPending(id)
}

// queueCustomViewBackfill queues the newest sources of a view's kinds, at
// most 500, written since the given time. Repository states are queued
// whether or not their README has blocks; the handler reads it.
func (t *Tenant) queueCustomViewBackfill(ctx context.Context, view customView, since int64, run string) (int, error) {
	rows, err := t.store.Query(ctx, event.Filter{Kinds: view.Kinds, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: viewBackfillLimit})
	if err != nil {
		return 0, err
	}
	var intents []storage.Intent
	for _, e := range rows.Events {
		if e.CreatedAt < since {
			continue
		}
		if e.Kind == event.KIND_REPO_STATE {
			if t.git == nil || t.statePending(ctx, e.ID) {
				continue
			}
		} else if len(views.Matching(views.Blocks(e.Content), view.Languages)) == 0 {
			continue
		}
		intents = append(intents, t.viewIntent(view, e, run))
	}
	if len(intents) == 0 {
		return 0, nil
	}
	err = t.store.WithTx(ctx, func(tx *sql.Tx) error {
		return storage.AddIntents(ctx, tx, intents, time.Now().Unix())
	})
	return len(intents), err
}

// tickCustomViews queues the hourly views that are due. It is called from
// the tenant scheduler on its own timer.
func (t *Tenant) tickCustomViews(ctx context.Context, now int64) error {
	rows, err := t.customViewRows(ctx)
	if err != nil {
		return err
	}
	for _, view := range rows {
		if !view.Enabled || view.Trigger != "hourly" || now-view.LastRunAt < viewHourlyPeriod {
			continue
		}
		since := view.LastRunAt - 300
		if view.LastRunAt == 0 {
			since = 0
		}
		queued, err := t.queueCustomViewBackfill(ctx, view, since, strconv.FormatInt(now/viewHourlyPeriod, 10))
		if err != nil {
			return err
		}
		if _, err := t.store.DB().ExecContext(ctx, `UPDATE custom_views SET last_run_at=? WHERE name=?`, now, view.Name); err != nil {
			return err
		}
		if queued > 0 {
			t.app.telemetry.Logger().Info("custom view hourly run queued", "tenant", t.meta.Name, "queued", queued)
		}
	}
	return nil
}
