package daemon

// Custom view transforms. Each source event is one POST of its fenced
// blocks to the view's transform, signed with the view's secret. What comes
// back is checked, signed by the relay and attached to the source. A failed
// POST is tried again after one minute and then after five; after 20
// failures in a row the view is paused until the owner resumes it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/views"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

// viewPayload is the work intent body: the source event, so the run does
// not depend on the event still being stored, the attempt number and the
// run that queued it.
type viewPayload struct {
	Event   event.Event `json:"event"`
	Attempt int         `json:"attempt"`
	Run     string      `json:"run,omitempty"`
}

// viewRequest is the body posted to a transform: only the blocks, with the
// source named so the transform can correlate. No event content, tags,
// author or time leaves the relay.
type viewRequest struct {
	Relay  string        `json:"relay"`
	View   string        `json:"view"`
	Source viewSource    `json:"source"`
	Blocks []views.Block `json:"blocks"`
}

type viewSource struct {
	ID   string `json:"id"`
	Kind int    `json:"kind"`
}

// viewResponse is what a transform answers with.
type viewResponse struct {
	Artifacts []viewArtifact `json:"artifacts"`
	Errors    []viewError    `json:"errors"`
}

type viewArtifact struct {
	Block  int    `json:"block"`
	Type   string `json:"type"`
	Body   string `json:"body"`
	Engine string `json:"engine"`
}

type viewError struct {
	Block int    `json:"block"`
	Error string `json:"error"`
}

// viewSourceKey names a source in the artifact tables: the event id, or the
// coordinate of a repository state so a new state replaces its README.
func viewSourceKey(e event.Event) string {
	if e.Kind == event.KIND_REPO_STATE {
		return strconv.Itoa(e.Kind) + ":" + e.PubKey + ":" + event.Tag(e, "d")
	}
	return e.ID
}

// viewSourceTag is the tag an artifact carries for a source.
func viewSourceTag(source string) []string {
	if strings.HasPrefix(source, strconv.Itoa(event.KIND_REPO_STATE)+":") {
		return []string{"a", source}
	}
	return []string{"e", source}
}

// viewHTTPClient refuses private addresses and redirects unless the relay
// itself runs on a loopback address.
func (t *Tenant) viewHTTPClient() *http.Client {
	if t.viewClient != nil {
		return t.viewClient
	}
	if t.loopbackRelay() {
		return &http.Client{Timeout: viewTimeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return replication.NewPinnedClient(viewTimeout)
}

// viewLock bounds work to one transform run per view at a time.
func (t *Tenant) viewLock(name string) *sync.Mutex {
	t.viewMu.Lock()
	defer t.viewMu.Unlock()
	if t.viewLocks == nil {
		t.viewLocks = map[string]*sync.Mutex{}
	}
	lock := t.viewLocks[name]
	if lock == nil {
		lock = &sync.Mutex{}
		t.viewLocks[name] = lock
	}
	return lock
}

// sourceBlocks finds the blocks of a source: the fenced blocks of its
// content, or of the README at the head of a repository state.
func (t *Tenant) sourceBlocks(ctx context.Context, e event.Event) ([]views.Block, error) {
	if e.Kind != event.KIND_REPO_STATE {
		return views.Blocks(e.Content), nil
	}
	readme, err := t.stateReadme(ctx, e)
	if err != nil {
		return nil, err
	}
	return views.Blocks(readme), nil
}

// stateReadme reads README.md at the head named by a repository state. A
// missing README yields no content.
func (t *Tenant) stateReadme(ctx context.Context, e event.Event) (string, error) {
	if t.git == nil {
		return "", nil
	}
	head := strings.TrimPrefix(event.Tag(e, "HEAD"), "ref: ")
	commit := ""
	for _, tag := range e.Tags {
		if len(tag) >= 2 && tag[0] == head {
			commit = tag[1]
		}
	}
	if head == "" || commit == "" {
		return "", nil
	}
	for _, name := range []string{"README.md", "readme.md", "README"} {
		page, err := t.git.Browse(ctx, gitrelay.BrowseRequest{Owner: e.PubKey, Repo: event.Tag(e, "d"), Ref: commit, Path: name, View: "file", Limit: 1})
		if err != nil {
			if strings.HasPrefix(err.Error(), "not found:") || strings.HasPrefix(err.Error(), "invalid:") {
				continue
			}
			return "", err
		}
		if page.Binary || page.Content == "" {
			continue
		}
		return page.Content, nil
	}
	return "", nil
}

// sourceCurrent reports whether the source is still the stored event: an
// event that was deleted, or a state that a newer state replaced, renders
// nothing.
func (t *Tenant) sourceCurrent(ctx context.Context, e event.Event) (bool, error) {
	var id string
	var err error
	if e.Kind == event.KIND_REPO_STATE {
		err = t.store.DB().QueryRowContext(ctx, `SELECT id FROM events WHERE kind=? AND pubkey=? AND d=?`, e.Kind, e.PubKey, event.Tag(e, "d")).Scan(&id)
	} else {
		err = t.store.DB().QueryRowContext(ctx, `SELECT id FROM events WHERE id=?`, e.ID).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return id == e.ID, err
}

// handleViewTransform serves one view-transform intent. It rechecks the
// view and the source right before the POST, attaches artifacts that
// already exist for a block's hash and posts only the blocks that need
// rendering.
func (t *Tenant) handleViewTransform(ctx context.Context, intent work.Intent) (err error) {
	ctx, finish := t.app.telemetry.Start(ctx, "view")
	outcome := "error"
	defer func() { finish(outcome) }()
	var payload viewPayload
	if err := json.Unmarshal([]byte(intent.Payload), &payload); err != nil || payload.Event.ID == "" {
		outcome = "invalid"
		return errors.New("view-transform: invalid payload")
	}
	if payload.Attempt < 1 {
		payload.Attempt = 1
	}
	lock := t.viewLock(intent.Target)
	lock.Lock()
	defer lock.Unlock()
	view, err := t.customViewByName(ctx, intent.Target)
	if err != nil {
		if strings.HasPrefix(err.Error(), "not found:") {
			outcome = "paused"
			return nil
		}
		return err
	}
	if !view.Enabled {
		outcome = "paused"
		return nil
	}
	current, err := t.sourceCurrent(ctx, payload.Event)
	if err != nil {
		return err
	}
	if !current {
		outcome = "invalid"
		return nil
	}
	blocks, err := t.sourceBlocks(ctx, payload.Event)
	if err != nil {
		return err
	}
	blocks = views.Matching(blocks, view.Languages)
	source := viewSourceKey(payload.Event)
	expires := event.Expiration(payload.Event)
	// Attach the blocks that already have an artifact and keep the rest.
	var pending []views.Block
	hashes := make([]string, 0, len(blocks))
	for _, block := range blocks {
		hash := views.Hash(block.Lang, block.Source)
		hashes = append(hashes, hash)
		exists, err := t.attachArtifact(ctx, view, hash, source, block.Index, expires)
		if err != nil {
			return err
		}
		if !exists {
			pending = append(pending, block)
		}
	}
	if err := t.detachStaleArtifacts(ctx, view.Name, source, hashes); err != nil {
		return err
	}
	if len(pending) == 0 {
		outcome = "ok"
		return t.pruneViewArtifacts(ctx, view.Name, intent.ID)
	}
	status, response, postErr := t.postViewTransform(ctx, view, payload.Event, pending)
	now := time.Now().Unix()
	if postErr == nil {
		stored, refused := 0, 0
		sent := map[int]views.Block{}
		for _, block := range pending {
			sent[block.Index] = block
		}
		for _, artifact := range response.Artifacts {
			block, ok := sent[artifact.Block]
			if !ok {
				refused++
				continue
			}
			if err := t.storeArtifact(ctx, view, block, artifact, source, expires, now); err != nil {
				if strings.HasPrefix(err.Error(), "invalid:") {
					refused++
					continue
				}
				return err
			}
			stored++
		}
		outcome = "ok"
		if refused > 0 {
			outcome = "invalid"
		}
		if _, err := t.store.DB().ExecContext(ctx, `UPDATE custom_views SET last_run_at=?, last_status='ok', failures=0 WHERE name=?`, now, view.Name); err != nil {
			return err
		}
		// Counts only: no source, block or body leaves the relay in a log.
		t.app.telemetry.Logger().Info("view transform delivered", "tenant", t.meta.Name, "attempt", payload.Attempt, "status", status, "blocks", len(pending), "artifacts", stored, "refused", refused, "errors", len(response.Errors))
		return t.pruneViewArtifacts(ctx, view.Name, intent.ID)
	}
	reason := postErr.Error()
	failures := view.Failures + 1
	paused := failures >= viewPauseFailures
	if paused {
		outcome = "paused"
		if err := t.pauseCustomView(ctx, view.Name, failures, fmt.Sprintf("paused after %d failures: %s", failures, reason)); err != nil {
			return err
		}
	} else if _, err := t.store.DB().ExecContext(ctx, `UPDATE custom_views SET last_run_at=?, failures=?, last_status=? WHERE name=?`, now, failures, reason, view.Name); err != nil {
		return err
	}
	t.app.telemetry.Logger().Info("view transform failed", "tenant", t.meta.Name, "attempt", payload.Attempt, "status", status, "failures", failures, "paused", paused)
	if paused || payload.Attempt >= viewMaxAttempts {
		return nil
	}
	return t.retryViewTransform(ctx, intent, payload)
}

// pauseCustomView stops the view and records the count and reason, then
// drops the index so no new intents are queued for it.
func (t *Tenant) pauseCustomView(ctx context.Context, name string, failures int, status string) error {
	if _, err := t.store.DB().ExecContext(ctx, `UPDATE custom_views SET enabled=0, failures=?, last_status=? WHERE name=?`, failures, status, name); err != nil {
		return err
	}
	t.invalidateCustomViews()
	return nil
}

// retryViewTransform queues the next attempt after its backoff as its own
// intent so the durable queue's ordering and fencing still apply.
func (t *Tenant) retryViewTransform(ctx context.Context, intent work.Intent, payload viewPayload) error {
	delay := viewBackoff[len(viewBackoff)-1]
	if payload.Attempt-1 < len(viewBackoff) {
		delay = viewBackoff[payload.Attempt-1]
	}
	payload.Attempt++
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	key := sha256.Sum256([]byte(viewTransform + "\x00" + intent.EventID + "\x00" + intent.Target + "\x00" + fmt.Sprint(payload.Attempt)))
	return t.store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, hex.EncodeToString(key[:]), viewTransform, intent.EventID+"#"+fmt.Sprint(payload.Attempt), intent.Target, string(encoded), now+int64(delay/time.Second), now, now)
		return err
	})
}

// postViewTransform sends the blocks and reads the answer. It returns the
// HTTP status with a bounded reason on failure; a response that is not
// JSON counts as a failure.
func (t *Tenant) postViewTransform(ctx context.Context, view customView, e event.Event, blocks []views.Block) (int, viewResponse, error) {
	body, err := json.Marshal(viewRequest{Relay: t.publicURL, View: view.Name, Source: viewSource{ID: viewSourceKey(e), Kind: e.Kind}, Blocks: blocks})
	if err != nil {
		return 0, viewResponse{}, errors.New("encode request")
	}
	ctx, cancel := context.WithTimeout(ctx, viewTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, view.Transform, bytes.NewReader(body))
	if err != nil {
		return 0, viewResponse{}, errors.New("bad url")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Tiny-View", view.Name)
	request.Header.Set("X-Tiny-Relay", t.publicURL)
	request.Header.Set("X-Tiny-Signature", callbackSignature(view.secret, body))
	request.Header.Set("User-Agent", "tinyrelay")
	response, err := t.viewHTTPClient().Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return 0, viewResponse{}, errors.New("timeout")
		}
		return 0, viewResponse{}, errors.New("connection failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, viewResponseMaxBytes))
	if err != nil {
		return response.StatusCode, viewResponse{}, errors.New("read failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, viewResponse{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var decoded viewResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return response.StatusCode, viewResponse{}, errors.New("invalid response")
	}
	return response.StatusCode, decoded, nil
}

var (
	svgScript        = regexp.MustCompile(`(?i)<script`)
	svgHandler       = regexp.MustCompile(`(?i)\bon[a-z]+\s*=`)
	svgForeignObject = regexp.MustCompile(`(?i)<foreignObject`)
	svgReference     = regexp.MustCompile(`(?i)(?:xlink:)?href\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	pngSignature     = []byte("\x89PNG\r\n\x1a\n")
)

// checkArtifact validates a transform's artifact: an accepted type, a body
// within the view's size limit, a PNG that decodes from base64 and an SVG
// without script, handlers, foreign objects or references outside itself.
// It returns the body as stored: the SVG text or the PNG in base64.
func checkArtifact(artifact viewArtifact, maxBytes int) (string, error) {
	switch artifact.Type {
	case "image/svg+xml":
		if len(artifact.Body) > maxBytes {
			return "", errors.New("invalid: artifact exceeds the size limit")
		}
		if err := checkSVG(artifact.Body); err != nil {
			return "", err
		}
		return artifact.Body, nil
	case "image/png":
		body := strings.TrimSpace(artifact.Body)
		decoded, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			if decoded, err = base64.RawStdEncoding.DecodeString(body); err != nil {
				return "", errors.New("invalid: png body must be base64")
			}
		}
		if len(decoded) > maxBytes {
			return "", errors.New("invalid: artifact exceeds the size limit")
		}
		if !bytes.HasPrefix(decoded, pngSignature) {
			return "", errors.New("invalid: png body is not a PNG")
		}
		return base64.StdEncoding.EncodeToString(decoded), nil
	default:
		return "", errors.New("invalid: artifact type must be image/svg+xml or image/png")
	}
}

func checkSVG(body string) error {
	if !strings.Contains(strings.ToLower(body), "<svg") {
		return errors.New("invalid: svg body has no svg element")
	}
	if svgScript.MatchString(body) {
		return errors.New("invalid: svg contains script")
	}
	if svgHandler.MatchString(body) {
		return errors.New("invalid: svg contains an event handler")
	}
	if svgForeignObject.MatchString(body) {
		return errors.New("invalid: svg contains a foreign object")
	}
	for _, match := range svgReference.FindAllStringSubmatch(body, -1) {
		target := strings.TrimSpace(match[1] + match[2] + match[3])
		if target == "" || strings.HasPrefix(target, "#") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(target), "javascript:") {
			return errors.New("invalid: svg contains a javascript reference")
		}
		return errors.New("invalid: svg references an external target")
	}
	return nil
}

// storeArtifact checks one artifact and keeps it as a signed record
// attached to its source.
func (t *Tenant) storeArtifact(ctx context.Context, view customView, block views.Block, artifact viewArtifact, source string, expires int64, now int64) error {
	content, err := checkArtifact(artifact, view.MaxBytes)
	if err != nil {
		return err
	}
	engine := strings.TrimSpace(artifact.Engine)
	if len(engine) > 64 {
		engine = engine[:64]
	}
	hash := views.Hash(block.Lang, block.Source)
	err = t.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO custom_view_artifacts(view,hash,event_id,type,engine,audience,content,raw,created_at) VALUES(?,?,'',?,?,?,?,'',?) ON CONFLICT(view,hash) DO UPDATE SET type=excluded.type, engine=excluded.engine, audience=excluded.audience, content=excluded.content, created_at=excluded.created_at`, view.Name, hash, artifact.Type, engine, view.Audience, content, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO custom_view_sources(view,hash,source,block,expires) VALUES(?,?,?,?,?) ON CONFLICT(view,hash,source) DO UPDATE SET block=excluded.block, expires=excluded.expires`, view.Name, hash, source, block.Index, expires)
		return err
	})
	if err != nil {
		return err
	}
	return t.signArtifact(ctx, view, hash, now)
}

// attachArtifact reports whether an artifact exists for the hash and, when
// it does, attaches the source to it through a fresh signed record.
func (t *Tenant) attachArtifact(ctx context.Context, view customView, hash, source string, block int, expires int64) (bool, error) {
	var exists int
	err := t.store.DB().QueryRowContext(ctx, `SELECT 1 FROM custom_view_artifacts WHERE view=? AND hash=?`, view.Name, hash).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var current int
	var currentBlock int
	var currentExpires int64
	err = t.store.DB().QueryRowContext(ctx, `SELECT 1,block,expires FROM custom_view_sources WHERE view=? AND hash=? AND source=?`, view.Name, hash, source).Scan(&current, &currentBlock, &currentExpires)
	if err == nil && currentBlock == block && currentExpires == expires {
		return true, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if _, err := t.store.DB().ExecContext(ctx, `INSERT INTO custom_view_sources(view,hash,source,block,expires) VALUES(?,?,?,?,?) ON CONFLICT(view,hash,source) DO UPDATE SET block=excluded.block, expires=excluded.expires`, view.Name, hash, source, block, expires); err != nil {
		return false, err
	}
	return true, t.signArtifact(ctx, view, hash, time.Now().Unix())
}

// signArtifact signs the artifact with its current sources as a kind 30078
// record and stores the record when the view is public.
func (t *Tenant) signArtifact(ctx context.Context, view customView, hash string, now int64) error {
	var typ, engine, content string
	if err := t.store.DB().QueryRowContext(ctx, `SELECT type,engine,content FROM custom_view_artifacts WHERE view=? AND hash=?`, view.Name, hash).Scan(&typ, &engine, &content); err != nil {
		return err
	}
	rows, err := t.store.DB().QueryContext(ctx, `SELECT source,block,expires FROM custom_view_sources WHERE view=? AND hash=? ORDER BY rowid`, view.Name, hash)
	if err != nil {
		return err
	}
	tags := [][]string{{"-"}, {"d", "bind.ws/view/" + view.Name + "/" + hash}, {"view", view.Name}}
	var blocks [][]string
	expires := int64(0)
	unlimited := false
	for rows.Next() {
		var source string
		var block int
		var sourceExpires int64
		if err := rows.Scan(&source, &block, &sourceExpires); err != nil {
			rows.Close()
			return err
		}
		tags = append(tags, viewSourceTag(source))
		blocks = append(blocks, []string{"block", strconv.Itoa(block)})
		if sourceExpires == 0 {
			unlimited = true
		} else if sourceExpires > expires {
			expires = sourceExpires
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	tags = append(tags, blocks...)
	tags = append(tags, []string{"type", typ})
	if engine != "" {
		tags = append(tags, []string{"engine", engine})
	}
	if expires > 0 && !unlimited {
		tags = append(tags, []string{"expiration", strconv.FormatInt(expires, 10)})
	}
	var signed event.Event
	if view.Audience == "public" {
		signed, err = t.records.Generate(ctx, event.KIND_VIEW, tags, content, now)
	} else {
		signed, err = t.records.Sign(ctx, event.KIND_VIEW, tags, content, now)
	}
	if err != nil {
		return err
	}
	raw, err := json.Marshal(signed)
	if err != nil {
		return err
	}
	_, err = t.store.DB().ExecContext(ctx, `UPDATE custom_view_artifacts SET event_id=?, raw=? WHERE view=? AND hash=?`, signed.ID, string(raw), view.Name, hash)
	return err
}

// detachStaleArtifacts drops the source from artifacts whose block it no
// longer carries, then removes artifacts left without a source.
func (t *Tenant) detachStaleArtifacts(ctx context.Context, name, source string, hashes []string) error {
	rows, err := t.store.DB().QueryContext(ctx, `SELECT hash FROM custom_view_sources WHERE view=? AND source=?`, name, source)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, hash := range hashes {
		keep[hash] = true
	}
	var stale []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			return err
		}
		if !keep[hash] {
			stale = append(stale, hash)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, hash := range stale {
		if _, err := t.store.DB().ExecContext(ctx, `DELETE FROM custom_view_sources WHERE view=? AND hash=? AND source=?`, name, hash, source); err != nil {
			return err
		}
	}
	return t.dropOrphanArtifacts(ctx, name)
}

// pruneViewArtifacts detaches sources that no longer exist and removes the
// artifacts that lose their last source. A view with transform work still
// queued is left alone so a replaced source can reattach its blocks first.
func (t *Tenant) pruneViewArtifacts(ctx context.Context, name, except string) error {
	var pending int
	if err := t.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM work_intents WHERE kind=? AND target=? AND state IN ('pending','running') AND id<>?`, viewTransform, name, except).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}
	rows, err := t.store.DB().QueryContext(ctx, `SELECT DISTINCT source FROM custom_view_sources WHERE view=?`, name)
	if err != nil {
		return err
	}
	var sources []string
	for rows.Next() {
		var source string
		if err := rows.Scan(&source); err != nil {
			rows.Close()
			return err
		}
		sources = append(sources, source)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	removed := 0
	for _, source := range sources {
		present, err := t.sourcePresent(ctx, source)
		if err != nil {
			return err
		}
		if present {
			continue
		}
		if _, err := t.store.DB().ExecContext(ctx, `DELETE FROM custom_view_sources WHERE view=? AND source=?`, name, source); err != nil {
			return err
		}
		removed++
	}
	if removed == 0 {
		return nil
	}
	return t.dropOrphanArtifacts(ctx, name)
}

// pruneAllViewArtifacts runs the prune for every view; the maintenance
// sweep calls it after expired events are removed.
func (t *Tenant) pruneAllViewArtifacts(ctx context.Context) error {
	rows, err := t.customViewRows(ctx)
	if err != nil {
		return err
	}
	for _, view := range rows {
		if err := t.pruneViewArtifacts(ctx, view.Name, ""); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tenant) sourcePresent(ctx context.Context, source string) (bool, error) {
	var one int
	var err error
	if tag := viewSourceTag(source); tag[0] == "a" {
		parts := strings.SplitN(source, ":", 3)
		if len(parts) != 3 {
			return false, nil
		}
		err = t.store.DB().QueryRowContext(ctx, `SELECT 1 FROM events WHERE kind=? AND pubkey=? AND d=? LIMIT 1`, event.KIND_REPO_STATE, parts[1], parts[2]).Scan(&one)
	} else {
		err = t.store.DB().QueryRowContext(ctx, `SELECT 1 FROM events WHERE id=? AND (expires=0 OR expires>?) LIMIT 1`, source, time.Now().Unix()).Scan(&one)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// dropOrphanArtifacts removes artifacts without a source, and their stored
// records, then re-signs the artifacts whose source list shrank.
func (t *Tenant) dropOrphanArtifacts(ctx context.Context, name string) error {
	ids, err := t.customViewArtifactIDs(ctx, `SELECT event_id FROM custom_view_artifacts WHERE view=? AND NOT EXISTS(SELECT 1 FROM custom_view_sources WHERE custom_view_sources.view=custom_view_artifacts.view AND custom_view_sources.hash=custom_view_artifacts.hash)`, name)
	if err != nil {
		return err
	}
	if _, err := t.store.DB().ExecContext(ctx, `DELETE FROM custom_view_artifacts WHERE view=? AND NOT EXISTS(SELECT 1 FROM custom_view_sources WHERE custom_view_sources.view=custom_view_artifacts.view AND custom_view_sources.hash=custom_view_artifacts.hash)`, name); err != nil {
		return err
	}
	if err := t.deleteStoredArtifacts(ctx, ids); err != nil {
		return err
	}
	if len(ids) > 0 {
		t.app.telemetry.Logger().Info("view artifacts removed", "tenant", t.meta.Name, "removed", len(ids))
	}
	// Artifacts that kept a source but lost one carry a tag for it; a fresh
	// record drops it.
	view, err := t.customViewByName(ctx, name)
	if err != nil {
		return nil
	}
	rows, err := t.store.DB().QueryContext(ctx, `SELECT a.hash, a.raw FROM custom_view_artifacts a WHERE a.view=?`, name)
	if err != nil {
		return err
	}
	type stale struct{ hash string }
	var refresh []stale
	for rows.Next() {
		var hash, raw string
		if err := rows.Scan(&hash, &raw); err != nil {
			rows.Close()
			return err
		}
		var signed event.Event
		if json.Unmarshal([]byte(raw), &signed) != nil {
			continue
		}
		var tagged []string
		for _, tag := range signed.Tags {
			if len(tag) == 2 && (tag[0] == "e" || tag[0] == "a") {
				tagged = append(tagged, tag[1])
			}
		}
		current, err := t.artifactSources(ctx, name, hash)
		if err != nil {
			rows.Close()
			return err
		}
		sort.Strings(tagged)
		sort.Strings(current)
		if strings.Join(tagged, "\n") != strings.Join(current, "\n") {
			refresh = append(refresh, stale{hash})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, item := range refresh {
		if err := t.signArtifact(ctx, view, item.hash, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tenant) artifactSources(ctx context.Context, name, hash string) ([]string, error) {
	rows, err := t.store.DB().QueryContext(ctx, `SELECT source FROM custom_view_sources WHERE view=? AND hash=?`, name, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var source string
		if err := rows.Scan(&source); err != nil {
			return nil, err
		}
		out = append(out, source)
	}
	return out, rows.Err()
}

func (t *Tenant) customViewArtifactIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := t.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// deleteStoredArtifacts removes the public records of artifacts by id.
func (t *Tenant) deleteStoredArtifacts(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, err := t.store.DeleteEvent(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// cascadeViewDeletion runs after a kind 5 deletion is stored: artifacts
// attached to the deleted events or addresses lose that source at once.
func (t *Tenant) cascadeViewDeletion(ctx context.Context, e event.Event) {
	if e.Kind != event.KIND_DELETION {
		return
	}
	rows, err := t.customViewRows(ctx)
	if err != nil || len(rows) == 0 {
		return
	}
	for _, view := range rows {
		if err := t.pruneViewArtifacts(ctx, view.Name, ""); err != nil {
			t.app.telemetry.Logger().Error("view artifacts not pruned", "tenant", t.meta.Name, "error", err)
		}
	}
}

// artifactRecord is one stored artifact as the route serves it.
type artifactRecord struct {
	Type     string
	Audience string
	Content  string
	EventID  string
}

func (t *Tenant) customViewArtifact(ctx context.Context, name, hash string) (artifactRecord, error) {
	var record artifactRecord
	err := t.store.DB().QueryRowContext(ctx, `SELECT type,audience,content,event_id FROM custom_view_artifacts WHERE view=? AND hash=?`, name, hash).Scan(&record.Type, &record.Audience, &record.Content, &record.EventID)
	if errors.Is(err, sql.ErrNoRows) {
		return artifactRecord{}, errors.New("not found: artifact")
	}
	return record, err
}

// body returns the bytes served for the artifact.
func (r artifactRecord) body() ([]byte, error) {
	if r.Type == "image/png" {
		return base64.StdEncoding.DecodeString(r.Content)
	}
	return []byte(r.Content), nil
}
