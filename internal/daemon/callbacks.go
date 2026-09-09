package daemon

// Callbacks wake agents that cannot hold a relay connection open. A key
// registers an https URL with a filter, and the relay POSTs each matching
// event the key may see to that URL as it arrives. Registration is a
// management method; delivery is durable work with bounded retries. This
// file holds storage, validation, matching and the management methods;
// callbacks_delivery.go holds the POST.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const (
	callbackDelivery      = "callback-delivery"
	callbackTagMax        = 8
	callbackKindMax       = 32
	callbackURLMax        = 2048
	callbackSecretMin     = 16
	callbackSecretMax     = 128
	callbackTimeout       = 10 * time.Second
	callbackMaxAttempts   = 3
	callbackPauseFailures = 20
)

// callbackBackoff is the wait before the second and third attempt.
var callbackBackoff = []time.Duration{time.Minute, 5 * time.Minute}

const callbackSchema = `CREATE TABLE IF NOT EXISTS callbacks(id TEXT PRIMARY KEY, owner TEXT NOT NULL, url TEXT NOT NULL, filter TEXT NOT NULL, secret TEXT NOT NULL, created_at INTEGER NOT NULL, last_delivery_at INTEGER NOT NULL DEFAULT 0, last_status TEXT NOT NULL DEFAULT '', failures INTEGER NOT NULL DEFAULT 0, paused INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS callbacks_owner ON callbacks(owner);`

// callbackFilter is the NIP-01 subset a callback may use: kinds, authors and
// the a, e, p and h tags. since is accepted and ignored.
type callbackFilter struct {
	Kinds   []int               `json:"kinds"`
	Authors []string            `json:"authors,omitempty"`
	Tags    map[string][]string `json:"-"`
}

var callbackFilterTags = []string{"a", "e", "p", "h"}

// MarshalJSON writes the filter in NIP-01 form, with tag lists as #a, #e,
// #p and #h.
func (f callbackFilter) MarshalJSON() ([]byte, error) {
	out := map[string]any{"kinds": f.Kinds}
	if len(f.Authors) > 0 {
		out["authors"] = f.Authors
	}
	for _, name := range callbackFilterTags {
		if values := f.Tags[name]; len(values) > 0 {
			out["#"+name] = values
		}
	}
	return json.Marshal(out)
}

// eventFilter is the storage filter that matches the same events.
func (f callbackFilter) eventFilter() event.Filter {
	tags := map[string][]string{}
	for name, values := range f.Tags {
		tags[name] = values
	}
	return event.Filter{Kinds: f.Kinds, Authors: f.Authors, Tags: tags}
}

// parseCallbackFilter validates the filter subset. Unknown keys, empty
// lists, malformed values and lists longer than the cap are refused so a
// registration that will never match is caught when it is made.
func parseCallbackFilter(raw json.RawMessage) (callbackFilter, error) {
	var values map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil {
		// A filter may arrive as a JSON string from a plain form.
		var text string
		if json.Unmarshal(raw, &text) != nil || json.Unmarshal([]byte(text), &values) != nil {
			return callbackFilter{}, errors.New("invalid: filter must be a JSON object")
		}
	}
	f := callbackFilter{Tags: map[string][]string{}}
	for key, value := range values {
		switch {
		case key == "kinds":
			if json.Unmarshal(value, &f.Kinds) != nil || len(f.Kinds) == 0 || len(f.Kinds) > callbackKindMax {
				return callbackFilter{}, fmt.Errorf("invalid: kinds must list 1 to %d kinds", callbackKindMax)
			}
			for _, kind := range f.Kinds {
				if kind < 0 || kind > 65535 {
					return callbackFilter{}, errors.New("invalid: kinds must be between 0 and 65535")
				}
			}
		case key == "authors":
			if json.Unmarshal(value, &f.Authors) != nil || len(f.Authors) == 0 || len(f.Authors) > callbackTagMax {
				return callbackFilter{}, fmt.Errorf("invalid: authors must list 1 to %d public keys", callbackTagMax)
			}
			for _, author := range f.Authors {
				if !hex64(author) {
					return callbackFilter{}, errors.New("invalid: authors must be 64 lowercase hex characters")
				}
			}
		case key == "since":
			// Accepted for compatibility with clients that send whole
			// filters; a callback only sees events that arrive after it.
		case len(key) == 2 && key[0] == '#' && containsString(callbackFilterTags, key[1:]):
			var list []string
			if json.Unmarshal(value, &list) != nil || len(list) == 0 || len(list) > callbackTagMax {
				return callbackFilter{}, fmt.Errorf("invalid: %s must list 1 to %d values", key, callbackTagMax)
			}
			for _, item := range list {
				if err := checkCallbackTagValue(key[1:], item); err != nil {
					return callbackFilter{}, err
				}
			}
			f.Tags[key[1:]] = list
		default:
			return callbackFilter{}, fmt.Errorf("invalid: filter key %q is not supported; use kinds, authors, #a, #e, #p or #h", key)
		}
	}
	if len(f.Kinds) == 0 {
		return callbackFilter{}, errors.New("invalid: filter must name at least one kind")
	}
	sort.Ints(f.Kinds)
	return f, nil
}

func checkCallbackTagValue(name, value string) error {
	switch name {
	case "e", "p":
		if !hex64(value) {
			return fmt.Errorf("invalid: #%s values must be 64 lowercase hex characters", name)
		}
	case "a":
		parts := strings.SplitN(value, ":", 3)
		if len(parts) != 3 || !hex64(parts[1]) || strings.Trim(parts[0], "0123456789") != "" || parts[0] == "" {
			return errors.New("invalid: #a values must be <kind>:<pubkey>:<identifier>")
		}
	case "h":
		if !community.ValidRoomID(value) {
			return errors.New("invalid: #h values must be room ids")
		}
	}
	return nil
}

// callbackRecord is one stored callback. The secret never leaves the
// tenant except once, in the addcallback answer.
type callbackRecord struct {
	ID         string         `json:"id"`
	Owner      string         `json:"owner"`
	URL        string         `json:"url"`
	Filter     callbackFilter `json:"filter"`
	CreatedAt  int64          `json:"created"`
	LastAt     int64          `json:"lastDelivery"`
	LastStatus string         `json:"lastStatus"`
	Failures   int            `json:"failures"`
	Paused     bool           `json:"paused"`
	secret     string
}

// summary is the management view: the host stands in for the URL so a
// listing shown to moderators does not expose the path.
func (c callbackRecord) summary() map[string]any {
	return map[string]any{"id": c.ID, "owner": c.Owner, "url": c.URL, "host": callbackHost(c.URL), "filter": c.Filter, "created": c.CreatedAt, "lastDelivery": c.LastAt, "lastStatus": c.LastStatus, "failures": c.Failures, "paused": c.Paused}
}

func callbackHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func (t *Tenant) initCallbacks(ctx context.Context) error {
	_, err := t.store.DB().ExecContext(ctx, callbackSchema)
	return err
}

// checkCallbackURL accepts https URLs to public hosts. When the relay itself
// runs on a loopback address, as it does in tests, private targets are
// allowed so a local receiver can be exercised.
func (t *Tenant) checkCallbackURL(raw string) error {
	if len(raw) > callbackURLMax {
		return errors.New("invalid: url is too long")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("invalid: url must be https without credentials or a fragment")
	}
	if t.loopbackRelay() {
		return nil
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || !strings.Contains(host, ".") {
		return errors.New("invalid: url must name a public host")
	}
	if ip := net.ParseIP(host); ip != nil && privateAddress(ip) {
		return errors.New("invalid: url must not name a private or loopback address")
	}
	return nil
}

func privateAddress(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	_, shared, _ := net.ParseCIDR("100.64.0.0/10")
	return shared.Contains(ip)
}

// loopbackRelay reports whether the relay's own public URL is a loopback or
// private address, which only happens in tests and local trials.
func (t *Tenant) loopbackRelay() bool {
	parsed, err := url.Parse(t.publicURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && privateAddress(ip)
}

func callbackMethod(method string) bool {
	return containsString([]string{"listcallbacks", "addcallback", "removecallback", "pausecallback", "resumecallback"}, method)
}

// callbackExecute serves the callback management methods. Members and agents
// manage callbacks for their own key; the owner and moderators see and
// control every callback. Each call runs under the callback operation.
func (t *Tenant) callbackExecute(ctx context.Context, actor, method string, params []json.RawMessage) (result any, err error) {
	ctx, finish := t.app.telemetry.Start(ctx, "callback")
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
	if role == "" {
		return nil, fmt.Errorf("restricted: guest may not %s", method)
	}
	operator := role == "owner" || role == "moderator"
	switch method {
	case "listcallbacks":
		var rows []callbackRecord
		if operator {
			rows, err = t.callbackRows(ctx, "")
		} else {
			rows, err = t.callbackRows(ctx, actor)
		}
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			item := row.summary()
			if row.Owner != actor {
				// Operators see whose callback it is and where it goes,
				// not the path, which may carry a token.
				delete(item, "url")
			}
			out = append(out, item)
		}
		outcome = "ok"
		return out, nil
	case "addcallback":
		result, err = t.addCallback(ctx, actor, role, params)
	default:
		result, err = t.changeCallback(ctx, actor, operator, method, params)
	}
	if err == nil {
		outcome = "ok"
	}
	return result, err
}

func callbackOutcome(err error) string {
	switch {
	case strings.HasPrefix(err.Error(), "invalid:"), strings.HasPrefix(err.Error(), "not found:"):
		return "invalid"
	case strings.HasPrefix(err.Error(), "restricted:"):
		return "unauthorized"
	default:
		return "error"
	}
}

func (t *Tenant) addCallback(ctx context.Context, actor, role string, params []json.RawMessage) (any, error) {
	if len(params) != 1 {
		return nil, errors.New("invalid: addcallback expects one object with url, filter and an optional secret")
	}
	var options struct {
		URL    string          `json:"url"`
		Filter json.RawMessage `json:"filter"`
		Secret string          `json:"secret"`
	}
	if err := json.Unmarshal(params[0], &options); err != nil {
		return nil, errors.New("invalid: addcallback expects one object with url, filter and an optional secret")
	}
	options.URL = strings.TrimSpace(options.URL)
	if err := t.checkCallbackURL(options.URL); err != nil {
		return nil, err
	}
	filter, err := parseCallbackFilter(options.Filter)
	if err != nil {
		return nil, err
	}
	if role == "agent" {
		if err := t.checkCallbackScope(ctx, actor, filter); err != nil {
			return nil, err
		}
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
	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(filter)
	if err != nil {
		return nil, err
	}
	limit := t.Policy().Callbacks
	now := time.Now().Unix()
	err = t.store.WithTx(ctx, func(tx *sql.Tx) error {
		if role != "owner" {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM callbacks WHERE owner=?`, actor).Scan(&count); err != nil {
				return err
			}
			if count >= limit {
				return fmt.Errorf("invalid: at most %d callbacks per key", limit)
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO callbacks(id,owner,url,filter,secret,created_at) VALUES(?,?,?,?,?,?)`, id, actor, options.URL, string(encoded), secret, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	t.invalidateCallbacks()
	if err := t.community.Record(ctx, actor, "addcallback", id, ""); err != nil {
		return nil, err
	}
	// Counts only: the host, path and secret stay out of the log.
	t.app.telemetry.Logger().Info("callback registered", "tenant", t.meta.Name, "kinds", len(filter.Kinds), "generated", generated)
	record := callbackRecord{ID: id, Owner: actor, URL: options.URL, Filter: filter, CreatedAt: now}
	out := record.summary()
	out["secret"] = secret
	return out, nil
}

// checkCallbackScope keeps an agent's callback inside its grant: a room it
// names must be one it may post in and a repository it names must be one
// the grant covers. Kinds are not restricted, so an agent can listen for
// answers it may not publish itself.
func (t *Tenant) checkCallbackScope(ctx context.Context, actor string, filter callbackFilter) error {
	grant, ok, err := t.community.AgentGrant(ctx, actor)
	if err != nil {
		return err
	}
	if !ok || !grant.Active(time.Now().Unix()) {
		return errors.New("restricted: agent grant is not active")
	}
	for _, room := range filter.Tags["h"] {
		if !grant.AllowsRoom(room) {
			return fmt.Errorf("restricted: agent grant does not cover room %s", room)
		}
	}
	for _, coordinate := range filter.Tags["a"] {
		owner, identifier, ok := repoCoordinate(coordinate)
		if !ok {
			continue
		}
		if grant.RepoLevel(owner, identifier) == "" {
			return fmt.Errorf("restricted: agent grant does not cover repository %s", identifier)
		}
	}
	return nil
}

func (t *Tenant) changeCallback(ctx context.Context, actor string, operator bool, method string, params []json.RawMessage) (any, error) {
	id := callbackIDParam(params)
	if id == "" {
		return nil, errors.New("invalid: callback id required")
	}
	record, err := t.callback(ctx, id)
	if err != nil {
		return nil, err
	}
	if !operator && record.Owner != actor {
		return nil, fmt.Errorf("restricted: only the callback's owner, the relay owner or a moderator may %s", method)
	}
	switch method {
	case "removecallback":
		_, err = t.store.DB().ExecContext(ctx, `DELETE FROM callbacks WHERE id=?`, id)
	case "pausecallback":
		_, err = t.store.DB().ExecContext(ctx, `UPDATE callbacks SET paused=1, last_status='paused' WHERE id=?`, id)
		record.Paused, record.LastStatus = true, "paused"
	case "resumecallback":
		_, err = t.store.DB().ExecContext(ctx, `UPDATE callbacks SET paused=0, failures=0, last_status='' WHERE id=?`, id)
		record.Paused, record.Failures, record.LastStatus = false, 0, ""
	default:
		return nil, fmt.Errorf("unsupported: management method %q", method)
	}
	if err != nil {
		return nil, err
	}
	t.invalidateCallbacks()
	if err := t.community.Record(ctx, actor, method, id, ""); err != nil {
		return nil, err
	}
	if method == "removecallback" {
		return map[string]any{"id": id, "removed": true}, nil
	}
	return record.summary(), nil
}

// callbackIDParam reads the id from a bare string or from {"id": ...}.
func callbackIDParam(params []json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	if id := stringParam(params, 0); id != "" {
		return id
	}
	var options struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(params[0], &options)
	return strings.TrimSpace(options.ID)
}

func (t *Tenant) callback(ctx context.Context, id string) (callbackRecord, error) {
	rows, err := t.callbackRows(ctx, "", id)
	if err != nil {
		return callbackRecord{}, err
	}
	if len(rows) == 0 {
		return callbackRecord{}, errors.New("not found: callback")
	}
	return rows[0], nil
}

// callbackRows lists callbacks, narrowed to an owner or to one id.
func (t *Tenant) callbackRows(ctx context.Context, owner string, ids ...string) ([]callbackRecord, error) {
	query := `SELECT id,owner,url,filter,secret,created_at,last_delivery_at,last_status,failures,paused FROM callbacks`
	args := []any{}
	switch {
	case len(ids) > 0:
		query += ` WHERE id=?`
		args = append(args, ids[0])
	case owner != "":
		query += ` WHERE owner=?`
		args = append(args, owner)
	}
	query += ` ORDER BY created_at,id`
	rows, err := t.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list callbacks: %w", err)
	}
	defer rows.Close()
	var out []callbackRecord
	for rows.Next() {
		var item callbackRecord
		var filter string
		var paused int
		if err := rows.Scan(&item.ID, &item.Owner, &item.URL, &filter, &item.secret, &item.CreatedAt, &item.LastAt, &item.LastStatus, &item.Failures, &paused); err != nil {
			return nil, err
		}
		item.Paused = paused != 0
		if item.Filter, err = parseCallbackFilter(json.RawMessage(filter)); err != nil {
			return nil, fmt.Errorf("callback filter: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// callbackRules returns the active callbacks indexed by kind. The index is
// built from the table on first use and dropped whenever a callback
// changes, so matching an event costs one map lookup.
func (t *Tenant) callbackRules(ctx context.Context) map[int][]callbackRecord {
	t.callbackMu.Lock()
	defer t.callbackMu.Unlock()
	if t.callbackIndex != nil {
		return t.callbackIndex
	}
	index := map[int][]callbackRecord{}
	rows, err := t.callbackRows(ctx, "")
	if err != nil {
		t.app.telemetry.Logger().Error("load callbacks", "tenant", t.meta.Name, "error", err)
		return index
	}
	for _, row := range rows {
		if row.Paused {
			continue
		}
		for _, kind := range row.Filter.Kinds {
			index[kind] = append(index[kind], row)
		}
	}
	t.callbackIndex = index
	return index
}

func (t *Tenant) invalidateCallbacks() {
	t.callbackMu.Lock()
	t.callbackIndex = nil
	t.callbackMu.Unlock()
}

// notifyCallbacks runs after an event is stored. Each active callback whose
// filter matches and whose key may see the event gets one delivery intent.
// It never fails the publish; a callback is best effort on top of the
// durable event.
func (t *Tenant) notifyCallbacks(ctx context.Context, e event.Event) {
	candidates := t.callbackRules(ctx)[e.Kind]
	if len(candidates) == 0 {
		return
	}
	var intents []storage.Intent
	unauthorized := 0
	for _, candidate := range candidates {
		if !event.Matches(candidate.Filter.eventFilter(), e) {
			continue
		}
		if !t.gate.CanSee(ctx, e, relay.Session{PubKeys: []string{candidate.Owner}, RelayURL: t.RelayURL()}, nil) {
			unauthorized++
			continue
		}
		payload, err := json.Marshal(callbackPayload{Event: e, Attempt: 1})
		if err != nil {
			continue
		}
		intents = append(intents, storage.Intent{Kind: callbackDelivery, EventID: e.ID, Target: candidate.ID, Payload: string(payload)})
	}
	if len(intents) == 0 {
		return
	}
	err := t.store.WithTx(ctx, func(tx *sql.Tx) error {
		return storage.AddIntents(ctx, tx, intents, time.Now().Unix())
	})
	if err != nil {
		t.app.telemetry.Logger().Error("callback deliveries not queued", "tenant", t.meta.Name, "error", err)
		return
	}
	t.app.telemetry.Logger().Debug("callback deliveries queued", "tenant", t.meta.Name, "queued", len(intents), "unauthorized", unauthorized)
}

func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("callback secret: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}
