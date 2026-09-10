package daemon

// Custom views hand fenced code blocks to an external transform and keep
// what comes back as relay-signed artifacts attached to the source event.
// This file holds the definition, its storage and the owner's management
// methods; custom_views_transform.go holds the block extraction, the POST
// and the artifacts, and custom_views_http.go serves them.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/records"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/telemetry"
	"github.com/FelineStateMachine/tinyrelay/internal/views"
)

// customViewService owns custom view definitions, transform execution and
// artifact storage. It borrows tenant dependencies and serializes transforms
// per view. Git is bound after repository initialization and before workers
// start; Records supplies the signing identity for generated artifacts.
type customViewService struct {
	store        *storage.Store
	community    *community.Service
	records      *records.Service
	git          *gitrelay.GitRelay
	telemetry    *telemetry.Telemetry
	tenantName   string
	publicURL    string
	loopback     func() bool
	resolveActor func(*http.Request) (string, error)
	browseRead   func(context.Context, string) error
	viewClient   *http.Client
	viewMu       sync.Mutex
	viewIndex    map[int][]customView
	viewLocks    map[string]*sync.Mutex
}

type customViewServiceConfig struct {
	Store        *storage.Store
	Community    *community.Service
	Records      *records.Service
	Git          *gitrelay.GitRelay
	Telemetry    *telemetry.Telemetry
	TenantName   string
	PublicURL    string
	Loopback     func() bool
	ResolveActor func(*http.Request) (string, error)
	BrowseRead   func(context.Context, string) error
}

func newCustomViewService(cfg customViewServiceConfig) *customViewService {
	return &customViewService{
		store:        cfg.Store,
		community:    cfg.Community,
		records:      cfg.Records,
		git:          cfg.Git,
		telemetry:    cfg.Telemetry,
		tenantName:   cfg.TenantName,
		publicURL:    cfg.PublicURL,
		loopback:     cfg.Loopback,
		resolveActor: cfg.ResolveActor,
		browseRead:   cfg.BrowseRead,
	}
}

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

const customViewSchema = `CREATE TABLE IF NOT EXISTS custom_views(name TEXT PRIMARY KEY, kinds TEXT NOT NULL, transform TEXT NOT NULL, trigger TEXT NOT NULL, audience TEXT NOT NULL, languages TEXT NOT NULL, max_bytes INTEGER NOT NULL, secret TEXT NOT NULL, generation TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, last_run_at INTEGER NOT NULL DEFAULT 0, last_status TEXT NOT NULL DEFAULT '', failures INTEGER NOT NULL DEFAULT 0);
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
	generation string
}

type customViewTarget struct {
	Name        string
	Fingerprint string
}

func customViewFingerprint(view customView) string {
	canonical := struct {
		Name       string
		Kinds      []int
		Transform  string
		Trigger    string
		Audience   string
		Languages  []string
		MaxBytes   int
		Secret     string
		CreatedAt  int64
		Generation string
	}{view.Name, view.Kinds, view.Transform, view.Trigger, view.Audience, view.Languages, view.MaxBytes, view.secret, view.CreatedAt, view.generation}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
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

func (t *customViewService) initCustomViews(ctx context.Context) error {
	if _, err := t.store.DB().ExecContext(ctx, customViewSchema); err != nil {
		return err
	}
	_, err := t.store.DB().ExecContext(ctx, `ALTER TABLE custom_views ADD COLUMN generation TEXT NOT NULL DEFAULT ''`)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return err
	}
	rows, err := t.store.DB().QueryContext(ctx, `SELECT name FROM custom_views WHERE generation=''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range names {
		generation, err := randomHex(16)
		if err != nil {
			return err
		}
		if _, err := t.store.DB().ExecContext(ctx, `UPDATE custom_views SET generation=? WHERE name=? AND generation=''`, generation, name); err != nil {
			return err
		}
	}
	return nil
}

func customViewMethod(method string) bool {
	return containsString([]string{"listcustomviews", "addcustomview", "removecustomview", "runcustomview", "pausecustomview", "resumecustomview"}, method)
}

// customViewExecute serves the owner's custom view methods under the view
// operation. Every change is recorded in the audit log.
func (t *customViewService) customViewExecute(ctx context.Context, actor, method string, params []json.RawMessage) (result any, err error) {
	ctx, finish := t.telemetry.Start(ctx, "view")
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

func (t *customViewService) addCustomView(ctx context.Context, actor string, params []json.RawMessage) (any, error) {
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
	view.generation, err = randomHex(16)
	if err != nil {
		return nil, err
	}
	view.Enabled = true
	view.CreatedAt = time.Now().Unix()
	kinds, _ := json.Marshal(view.Kinds)
	languages, _ := json.Marshal(view.Languages)
	_, err = t.store.DB().ExecContext(ctx, `INSERT INTO custom_views(name,kinds,transform,trigger,audience,languages,max_bytes,secret,generation,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,?,1,?)`, view.Name, string(kinds), view.Transform, view.Trigger, view.Audience, string(languages), view.MaxBytes, secret, view.generation, view.CreatedAt)
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
	t.telemetry.Logger().Info("custom view added", "tenant", t.tenantName, "kinds", len(view.Kinds), "languages", len(view.Languages), "generated", generated)
	out := view.summary()
	out["secret"] = secret
	return out, nil
}

// parseCustomView validates a definition: the name, the kinds it watches,
// the https transform, the trigger, the audience, the languages and the
// artifact size limit.
func (t *customViewService) parseCustomView(options customViewOptions) (customView, error) {
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
	if err := validateWebhookURL(view.Transform, t.publicURL); err != nil {
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

func (t *customViewService) changeCustomView(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
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
		refresh := customViewRefreshParam(params)
		queued, err := t.queueCustomViewBackfill(ctx, view, 0, strconv.FormatInt(time.Now().UnixNano(), 10), refresh)
		if err != nil {
			return nil, err
		}
		if err := t.community.Record(ctx, actor, method, name, strconv.Itoa(queued)); err != nil {
			return nil, err
		}
		t.telemetry.Logger().Info("custom view run queued", "tenant", t.tenantName, "queued", queued)
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
func (t *customViewService) removeCustomView(ctx context.Context, view customView) error {
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

func customViewRefreshParam(params []json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	var options struct {
		Refresh bool `json:"refresh"`
	}
	if err := json.Unmarshal(params[0], &options); err != nil {
		return false
	}
	return options.Refresh
}

func (t *customViewService) customViewByName(ctx context.Context, name string) (customView, error) {
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
func (t *customViewService) customViewRows(ctx context.Context, names ...string) ([]customView, error) {
	query := `SELECT name,kinds,transform,trigger,audience,languages,max_bytes,secret,generation,enabled,created_at,last_run_at,last_status,failures FROM custom_views`
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
		if err := rows.Scan(&item.Name, &kinds, &item.Transform, &item.Trigger, &item.Audience, &languages, &item.MaxBytes, &item.secret, &item.generation, &enabled, &item.CreatedAt, &item.LastRunAt, &item.LastStatus, &item.Failures); err != nil {
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
func (t *customViewService) customViewIndex(ctx context.Context) map[int][]customView {
	t.viewMu.Lock()
	defer t.viewMu.Unlock()
	if t.viewIndex != nil {
		return t.viewIndex
	}
	index := map[int][]customView{}
	rows, err := t.customViewRows(ctx)
	if err != nil {
		t.telemetry.Logger().Error("load custom views", "tenant", t.tenantName, "error", err)
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

func (t *customViewService) invalidateCustomViews() {
	t.viewMu.Lock()
	t.viewIndex = nil
	t.viewMu.Unlock()
}

// CustomViews is the summary the web UI renders with: the enabled views'
// names and languages, nothing else.
func (t *customViewService) CustomViews() []views.View {
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
func (t *customViewService) queueCustomViews(ctx context.Context, e event.Event) {
	intents := t.PrepareReleased(ctx, e)
	if len(intents) == 0 {
		return
	}
	err := t.store.WithTx(ctx, func(tx *sql.Tx) error {
		return storage.AddIntents(ctx, tx, intents, time.Now().Unix())
	})
	if err != nil {
		t.telemetry.Logger().Error("view transforms not queued", "tenant", t.tenantName, "error", err)
		return
	}
	t.telemetry.Logger().Debug("view transforms queued", "tenant", t.tenantName, "queued", len(intents))
}

// Prepare returns write-triggered transform intents for an event. The caller
// owns the transaction that persists the returned intents, allowing event and
// follow-up work to share one commit boundary.
func (t *customViewService) Prepare(ctx context.Context, e event.Event) []storage.Intent {
	targets, err := t.CandidateTargets(ctx, e)
	if err != nil {
		t.telemetry.Logger().Error("plan custom view transforms", "tenant", t.tenantName, "error", err)
		return nil
	}
	intents, err := t.PrepareTargets(ctx, e, targets, false)
	if err != nil {
		t.telemetry.Logger().Error("prepare custom view transforms", "tenant", t.tenantName, "error", err)
		return nil
	}
	return intents
}

// PrepareReleased returns intents after repository promotion has made Git
// objects available. Repository state events are intentionally accepted only
// through this path.
func (t *customViewService) PrepareReleased(ctx context.Context, e event.Event) []storage.Intent {
	targets, err := t.CandidateTargets(ctx, e)
	if err != nil {
		t.telemetry.Logger().Error("plan released custom view transforms", "tenant", t.tenantName, "error", err)
		return nil
	}
	intents, err := t.PrepareTargets(ctx, e, targets, true)
	if err != nil {
		t.telemetry.Logger().Error("prepare released custom view transforms", "tenant", t.tenantName, "error", err)
		return nil
	}
	return intents
}

// CandidateTargets captures names and registration identities for an event.
// The identity prevents a later registration with the same name from
// receiving work planned for an earlier registration.
func (t *customViewService) CandidateTargets(ctx context.Context, e event.Event) ([]customViewTarget, error) {
	if eventExpired(e, time.Now().Unix()) {
		return nil, nil
	}
	rows, err := t.customViewRows(ctx)
	if err != nil {
		return nil, err
	}
	targets := make([]customViewTarget, 0, len(rows))
	for _, view := range rows {
		if !view.Enabled || view.Trigger != "write" || !view.watches(e.Kind) {
			continue
		}
		if e.Kind != event.KIND_REPO_STATE && len(views.Matching(views.Blocks(e.Content), view.Languages)) == 0 {
			continue
		}
		targets = append(targets, customViewTarget{Name: view.Name, Fingerprint: customViewFingerprint(view)})
	}
	return targets, nil
}

// PrepareTargets rechecks captured registration identities before creating
// intents. A removed and recreated view with the same name is rejected.
func (t *customViewService) PrepareTargets(ctx context.Context, e event.Event, targets []customViewTarget, released bool) ([]storage.Intent, error) {
	if len(targets) == 0 || eventExpired(e, time.Now().Unix()) || (!released && e.Kind == event.KIND_REPO_STATE) {
		return nil, nil
	}
	if e.Kind != event.KIND_REPO_STATE {
		current, err := t.sourceCurrent(ctx, e)
		if err != nil {
			return nil, err
		}
		if !current {
			return nil, nil
		}
	}
	rows, err := t.customViewRows(ctx)
	if err != nil {
		return nil, err
	}
	candidates := make(map[string]customView, len(rows))
	for _, view := range rows {
		candidates[view.Name] = view
	}
	if e.Kind == event.KIND_REPO_STATE {
		if t.git == nil || t.statePending(ctx, e.ID) {
			return nil, nil
		}
	}
	var intents []storage.Intent
	for _, target := range targets {
		view, ok := candidates[target.Name]
		if !ok || !view.Enabled || view.Trigger != "write" || !view.watches(e.Kind) {
			continue
		}
		if customViewFingerprint(view) != target.Fingerprint {
			continue
		}
		if e.Kind != event.KIND_REPO_STATE && len(views.Matching(views.Blocks(e.Content), view.Languages)) == 0 {
			continue
		}
		intents = append(intents, t.viewIntent(view, e, "", false))
	}
	return intents, nil
}

func eventExpired(e event.Event, now int64) bool {
	expires := event.Expiration(e)
	return expires > 0 && expires <= now
}

// viewIntent is the transform intent for one source. The event travels in
// the payload; run makes a backfill or an hourly pass distinct from the
// write-time intent for the same event.
func (t *customViewService) viewIntent(view customView, e event.Event, run string, refresh ...bool) storage.Intent {
	force := len(refresh) > 0 && refresh[0]
	payload, _ := json.Marshal(viewPayload{Event: e, Attempt: 1, Run: run, Refresh: force, Generation: view.generation})
	id := e.ID
	if run != "" {
		id += "@" + run
	}
	return storage.Intent{Kind: viewTransform, EventID: id, Target: view.Name, Payload: string(payload)}
}

// statePending reports whether a repository state still waits for its
// objects.
func (t *customViewService) statePending(ctx context.Context, id string) bool {
	var one int
	err := t.store.DB().QueryRowContext(ctx, `SELECT 1 FROM pending_events WHERE id=?`, id).Scan(&one)
	return err == nil || t.git.IsPending(id)
}

// queueCustomViewBackfill queues the newest sources of a view's kinds, at
// most 500, written since the given time. Repository states are queued
// whether or not their README has blocks; the handler reads it.
func (t *customViewService) queueCustomViewBackfill(ctx context.Context, view customView, since int64, run string, refresh ...bool) (int, error) {
	force := len(refresh) > 0 && refresh[0]
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
		intents = append(intents, t.viewIntent(view, e, run, force))
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
func (t *customViewService) tickCustomViews(ctx context.Context, now int64) error {
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
		queued, err := t.queueCustomViewBackfill(ctx, view, since, strconv.FormatInt(now/viewHourlyPeriod, 10), false)
		if err != nil {
			return err
		}
		if _, err := t.store.DB().ExecContext(ctx, `UPDATE custom_views SET last_run_at=? WHERE name=?`, now, view.Name); err != nil {
			return err
		}
		if queued > 0 {
			t.telemetry.Logger().Info("custom view hourly run queued", "tenant", t.tenantName, "queued", queued)
		}
	}
	return nil
}
