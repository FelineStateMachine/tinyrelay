// Package records owns the durable records that a relay signs about itself.
// It deliberately keeps the relay secret inside the tenant settings table and
// exposes only public keys and generated events to callers.
package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type Config struct {
	Store               *storage.Store
	Policy              func() policy.Policy
	SetPolicy           func(policy.Policy) error
	RelayURL            string
	GroupID             string
	OnGenerated         func(context.Context, event.Event) error
	DeliverNotification func(context.Context, event.Event, string) error
	// PushNotification receives a plaintext summary of each notification for
	// device delivery. The gift wrap itself is opaque to the relay once sealed.
	PushNotification func(context.Context, string, string, string, string) error
	OnTransfer       func(context.Context, string, string) error
}

type Service struct {
	store               *storage.Store
	policy              func() policy.Policy
	setPolicy           func(policy.Policy) error
	relayURL            string
	groupID             string
	secret              string
	onGenerated         func(context.Context, event.Event) error
	deliverNotification func(context.Context, event.Event, string) error
	pushNotification    func(context.Context, string, string, string, string) error
	onTransfer          func(context.Context, string, string) error
	mu                  sync.Mutex
}

type MembershipChange struct {
	PubKey string
	Added  *bool
	Roles  []string
}

const secretSetting = "records.relay_secret"

const recordsSchema = `
CREATE TABLE IF NOT EXISTS records_audit(
 id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, actor TEXT NOT NULL,
 action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS records_presence(
 pubkey TEXT PRIMARY KEY, seen_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS records_pins(
 position INTEGER PRIMARY KEY, ref TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS records_view_runs(
 name TEXT NOT NULL, at INTEGER NOT NULL, rows INTEGER NOT NULL);
`

func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("records: nil store")
	}
	if cfg.Policy == nil {
		return nil, errors.New("records: nil policy function")
	}
	if _, err := cfg.Store.DB().ExecContext(ctx, recordsSchema); err != nil {
		return nil, fmt.Errorf("records schema: %w", err)
	}
	secret, err := loadOrCreateSecret(ctx, cfg.Store)
	if err != nil {
		return nil, err
	}
	groupID := cfg.GroupID
	if groupID == "" {
		groupID = cfg.RelayURL
	}
	return &Service{store: cfg.Store, policy: cfg.Policy, setPolicy: cfg.SetPolicy, relayURL: cfg.RelayURL, groupID: groupID, secret: secret, onGenerated: cfg.OnGenerated, deliverNotification: cfg.DeliverNotification, pushNotification: cfg.PushNotification, onTransfer: cfg.OnTransfer}, nil
}

func (s *Service) group() string { return s.groupID }

func groupMetaTags(group string, p policy.Policy) [][]string {
	tags := [][]string{{"-"}, {"d", group}, {"name", p.Name}, {"about", p.Description}}
	if p.Icon != "" {
		tags = append(tags, []string{"picture", p.Icon})
	}
	if p.Reads == "members" {
		tags = append(tags, []string{"private"})
	}
	if p.Writes != "open" {
		tags = append(tags, []string{"restricted"})
	}
	return tags
}

func loadOrCreateSecret(ctx context.Context, s *storage.Store) (string, error) {
	var secret string
	if err := s.GetSetting(ctx, secretSetting, &secret); err == nil {
		if len(secret) != 64 {
			return "", errors.New("records: invalid persisted relay secret")
		}
		return secret, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	secret, err := event.GenerateKey()
	if err != nil {
		return "", fmt.Errorf("records: generate relay key: %w", err)
	}
	if err := s.PutSetting(ctx, secretSetting, secret); err != nil {
		return "", err
	}
	return secret, nil
}

// PublicKey returns the relay signing key, never its secret.
func (s *Service) PublicKey() string {
	key, _ := event.PublicKey(s.secret)
	return key
}

func (s *Service) RelayURL() string { return s.relayURL }

func (s *Service) nextTimestamp(ctx context.Context, tx *sql.Tx, now int64) (int64, error) {
	var previous int64
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='records.clock'`).Scan(&previous); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if now <= previous {
		now = previous + 1
	}
	if err := storage.PutSetting(ctx, tx, "records.clock", now); err != nil {
		return 0, err
	}
	return now, nil
}

func (s *Service) signed(ctx context.Context, kind int, tags [][]string, content string, now int64) (event.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out event.Event
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		t, err := s.nextTimestamp(ctx, tx, now)
		if err != nil {
			return err
		}
		out = event.Event{CreatedAt: t, Kind: kind, Tags: tags, Content: content}
		return event.Sign(&out, s.secret)
	})
	if err != nil {
		return event.Event{}, err
	}
	if s.onGenerated != nil {
		if err := s.onGenerated(ctx, out); err != nil {
			return event.Event{}, err
		}
	}
	return out, nil
}

// signedOnly signs a projection without handing it to the persistence/live
// event callback. This is required for members-only folds and live presence:
// their audience is decided at request time and a generated event must never
// become a public durable record.
func (s *Service) signedOnly(ctx context.Context, kind int, tags [][]string, content string, now int64) (event.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out event.Event
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		t, err := s.nextTimestamp(ctx, tx, now)
		if err != nil {
			return err
		}
		out = event.Event{CreatedAt: t, Kind: kind, Tags: tags, Content: content}
		return event.Sign(&out, s.secret)
	})
	return out, err
}

func (s *Service) Generate(ctx context.Context, kind int, tags [][]string, content string, now int64) (event.Event, error) {
	return s.signed(ctx, kind, tags, content, now)
}

func (s *Service) profile(ctx context.Context, now int64) (event.Event, error) {
	p := s.policy()
	b, _ := json.Marshal(map[string]string{"name": p.Name, "about": p.Description, "picture": p.Icon})
	return s.signed(ctx, event.KIND_PROFILE, nil, string(b), now)
}

func (s *Service) discovery(ctx context.Context, now int64) (event.Event, error) {
	p := s.policy()
	tags := [][]string{{"d", s.relayURL}}
	if p.Reads != "open" {
		tags = append(tags, []string{"r", p.Reads})
	}
	if p.Writes != "open" {
		tags = append(tags, []string{"writes", p.Writes})
	}
	return s.signed(ctx, event.KIND_RELAY_DISCOVERY, tags, s.relayURL, now)
}

func (s *Service) roster(ctx context.Context, now int64) (event.Event, error) {
	tags := [][]string{{"-"}}
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,role FROM community_members ORDER BY pubkey`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var pk, role string
			if err := rows.Scan(&pk, &role); err != nil {
				return event.Event{}, err
			}
			tag := []string{"member", pk}
			if role != "" && role != "member" {
				tag = append(tag, role)
			}
			tags = append(tags, tag)
		}
	} else if !strings.Contains(err.Error(), "no such table") {
		return event.Event{}, err
	}
	if len(tags) == 1 && s.policy().Owner != "" {
		tags = append(tags, []string{"member", s.policy().Owner, "owner"})
	}
	return s.signed(ctx, event.KIND_ROSTER, tags, "", now)
}

func (s *Service) groupState(ctx context.Context, now int64) (event.Event, error) {
	p := s.policy()
	tags := [][]string{{"-"}, {"d", s.group()}, {"name", p.Name}, {"about", p.Description}}
	if p.Icon != "" {
		tags = append(tags, []string{"picture", p.Icon})
	}
	if p.Reads == "members" {
		tags = append(tags, []string{"private"})
	}
	if p.Writes != "open" {
		tags = append(tags, []string{"restricted"})
	}
	return s.signed(ctx, event.KIND_GROUP_METADATA, tags, "", now)
}

// MembershipDelta is the signed NIP-43 add/remove record emitted after a
// membership transaction. The community service remains the authority for
// admission; this service only publishes the relay's signed projection.
func (s *Service) MembershipDelta(ctx context.Context, pubkey string, added bool, now int64) (event.Event, error) {
	kind := event.KIND_MEMBER_REMOVED
	if added {
		kind = event.KIND_MEMBER_ADDED
	}
	return s.signed(ctx, kind, [][]string{{"-"}, {"p", pubkey}}, "", now)
}

func (s *Service) groupRecords(ctx context.Context, now int64) ([]event.Event, error) {
	p := s.policy()
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,role FROM community_members ORDER BY pubkey`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			rows = nil
		} else {
			return nil, err
		}
	}
	if rows == nil {
		return s.groupRecordsEmpty(ctx, now, p)
	}
	defer rows.Close()
	var members, admins, roleTags [][]string
	for rows.Next() {
		var pk, role string
		if err := rows.Scan(&pk, &role); err != nil {
			return nil, err
		}
		if role == "agent" {
			members = append(members, []string{"p", pk, role})
		} else {
			members = append(members, []string{"p", pk})
		}
		if role == "owner" || role == "moderator" {
			admins = append(admins, []string{"p", pk, role})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !p.DirectoryPublic {
		members = nil
	}
	for _, role := range roleOrder {
		roleTags = append(roleTags, []string{"role", role, rolesAbout[role]})
	}
	vals := []struct {
		kind    int
		tags    [][]string
		content string
	}{
		{event.KIND_GROUP_METADATA, groupMetaTags(s.group(), p), ""},
		{event.KIND_GROUP_ADMINS, append([][]string{{"-"}, {"d", s.group()}}, admins...), ""},
		{event.KIND_GROUP_MEMBERS, append([][]string{{"-"}, {"d", s.group()}}, members...), ""},
		{event.KIND_GROUP_ROLES, append([][]string{{"-"}, {"d", s.group()}}, roleTags...), ""},
	}
	out := make([]event.Event, 0, len(vals)+3)
	for _, v := range vals {
		e, err := s.signed(ctx, v.kind, v.tags, v.content, now)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	for i, role := range roleOrder {
		e, err := s.signed(ctx, event.KIND_ROLE_DEF, [][]string{{"-"}, {"d", role}, {"label", role}, {"description", rolesAbout[role]}, {"order", strconv.Itoa(i + 1)}}, "", now)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// roleOrder lists the roles the group advertises, in display order.
var roleOrder = []string{"owner", "moderator", "member", "agent"}
var rolesAbout = map[string]string{"owner": "relay owner", "moderator": "relay moderator", "member": "relay member", "agent": "agent under an owner-signed grant"}

func (s *Service) groupRecordsEmpty(ctx context.Context, now int64, p policy.Policy) ([]event.Event, error) {
	_ = p
	vals := []struct {
		kind    int
		tags    [][]string
		content string
	}{
		{event.KIND_GROUP_METADATA, groupMetaTags(s.group(), p), ""},
		{event.KIND_GROUP_ADMINS, [][]string{{"-"}, {"d", s.group()}}, ""},
		{event.KIND_GROUP_MEMBERS, [][]string{{"-"}, {"d", s.group()}}, ""},
		{event.KIND_GROUP_ROLES, [][]string{{"-"}, {"d", s.group()}}, ""},
		{event.KIND_ROLE_DEF, [][]string{{"-"}, {"d", "owner"}, {"label", "owner"}, {"description", "relay owner"}, {"order", "1"}}, ""},
		{event.KIND_ROLE_DEF, [][]string{{"-"}, {"d", "moderator"}, {"label", "moderator"}, {"description", "relay moderator"}, {"order", "2"}}, ""},
		{event.KIND_ROLE_DEF, [][]string{{"-"}, {"d", "member"}, {"label", "member"}, {"description", "relay member"}, {"order", "3"}}, ""},
		{event.KIND_ROLE_DEF, [][]string{{"-"}, {"d", "agent"}, {"label", "agent"}, {"description", "agent under an owner-signed grant"}, {"order", "4"}}, ""},
	}
	out := make([]event.Event, 0, len(vals))
	for _, v := range vals {
		e, err := s.signed(ctx, v.kind, v.tags, v.content, now)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Service) pins(ctx context.Context, now int64) (event.Event, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT ref FROM records_pins ORDER BY position`)
	if err != nil {
		return event.Event{}, err
	}
	defer rows.Close()
	tags := [][]string{{"-"}, {"d", s.group()}}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return event.Event{}, err
		}
		tags = append(tags, []string{"e", ref})
	}
	if err := rows.Err(); err != nil {
		return event.Event{}, err
	}
	return s.signed(ctx, event.KIND_GROUP_PINS, tags, "", now)
}

// PublishMembership emits the relay-owned identity and group records in a
// deterministic order. Persistence of the resulting events belongs to the
// OnGenerated callback so the relay's normal write gate remains authoritative.
func (s *Service) PublishMembership(ctx context.Context, now int64) ([]event.Event, error) {
	var out []event.Event
	for _, fn := range []func(context.Context, int64) (event.Event, error){s.profile, s.discovery, s.roster, s.groupState} {
		e, err := fn(ctx, now)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	group, err := s.groupRecords(ctx, now)
	if err != nil {
		return nil, err
	}
	out = append(out, group...)
	return out, nil
}

// PublishMembershipChanges extends the full projection with the NIP-29
// change records emitted by bindws for each membership mutation.
func (s *Service) PublishMembershipChanges(ctx context.Context, changes []MembershipChange, now int64) ([]event.Event, error) {
	out, err := s.PublishMembership(ctx, now)
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		if c.Added != nil {
			d, err := s.MembershipDelta(ctx, c.PubKey, *c.Added, now)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
		kind := event.KIND_PUT_USER
		if c.Added != nil && !*c.Added {
			kind = event.KIND_REMOVE_USER
		}
		tags := [][]string{{"-"}, {"h", s.group()}, {"p", c.PubKey}}
		for _, role := range c.Roles {
			tags[1] = append(tags[1], role)
		}
		e, err := s.signed(ctx, kind, tags, "", now)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Service) RecordPresence(ctx context.Context, pubkey string, now int64) error {
	if pubkey == "" {
		return errors.New("presence: empty pubkey")
	}
	_, err := s.store.DB().ExecContext(ctx, `INSERT INTO records_presence(pubkey,seen_at) VALUES(?,?) ON CONFLICT(pubkey) DO UPDATE SET seen_at=excluded.seen_at`, pubkey, now)
	return err
}

// NotePresence is the host hook for authenticated sockets and accepted writes.
// The daemon should call it on both paths, matching bindws notePresence.
func (s *Service) NotePresence(ctx context.Context, pubkey string, now int64) error {
	return s.RecordPresence(ctx, pubkey, now)
}

type Presence struct {
	PubKey string `json:"pubkey"`
	SeenAt int64  `json:"seen_at"`
}

func (s *Service) Presence(ctx context.Context, caller policy.Access, now int64) (event.Event, error) {
	if !policy.CanRead(s.policy(), event.Event{Kind: event.KIND_PRESENCE}, caller) {
		return event.Event{}, errors.New("restricted: presence")
	}
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,seen_at FROM records_presence WHERE seen_at>=? ORDER BY pubkey`, now-15*60)
	if err != nil {
		return event.Event{}, err
	}
	defer rows.Close()
	var values []Presence
	for rows.Next() {
		var p Presence
		if err := rows.Scan(&p.PubKey, &p.SeenAt); err != nil {
			return event.Event{}, err
		}
		values = append(values, p)
	}
	b, _ := json.Marshal(values)
	return s.signedOnly(ctx, event.KIND_PRESENCE, nil, string(b), now)
}

// SetPins persists ordered references and emits the signed group pin record.
func (s *Service) SetPins(ctx context.Context, refs []string, now int64) (event.Event, error) {
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM records_pins`); err != nil {
			return err
		}
		for i, ref := range refs {
			if strings.TrimSpace(ref) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO records_pins(position,ref) VALUES(?,?)`, i, ref); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return event.Event{}, err
	}
	return s.pins(ctx, now)
}

func (s *Service) audit(ctx context.Context, actor, action, target, detail string, now int64) error {
	_, err := s.store.DB().ExecContext(ctx, `INSERT INTO records_audit(at,actor,action,target,detail) VALUES(?,?,?,?,?)`, now, actor, action, target, detail)
	return err
}

type View struct {
	Name  string      `json:"name"`
	Event event.Event `json:"event"`
	Rows  int         `json:"rows"`
}

type ViewRun struct {
	At   int64 `json:"at"`
	Rows int   `json:"rows"`
}

var viewNames = []string{"profiles", "relays", "calendar", "moderation", "articles", "zaps", "presence"}

func effectiveViewTrigger(p policy.Policy, name string) string {
	if mode, ok := p.Views[name]; ok && mode != "" {
		return mode
	}
	return defaultViewTrigger(name)
}

func defaultViewTrigger(name string) string {
	return map[string]string{"profiles": "daily", "relays": "daily", "calendar": "hourly", "moderation": "daily", "articles": "write", "zaps": "hourly", "presence": "live"}[name]
}

// viewAudience mirrors the source view definitions. Directory visibility only
// controls profiles and relay lists; calendar/article/zap/moderation views use
// the relay read policy, and presence follows the same read policy.
func viewAudience(p policy.Policy, name string) string {
	if name == "profiles" || name == "relays" {
		if !p.DirectoryPublic {
			return "members"
		}
	}
	if p.Reads == "members" {
		return "members"
	}
	return "public"
}

func viewStored(p policy.Policy, name string) bool {
	return name != "presence" && viewAudience(p, name) == "public"
}

func (s *Service) Views(ctx context.Context, caller policy.Access, now int64) ([]View, error) {
	p := s.policy()
	if p.Reads == "members" && !caller.Member && !caller.Owner {
		return nil, errors.New("restricted: views")
	}
	if p.Reads == "auth" && len(caller.PubKeys) == 0 && !caller.Owner {
		return nil, errors.New("auth-required: views")
	}
	var out []View
	for _, name := range viewNames {
		if name == "presence" {
			continue
		}
		if effectiveViewTrigger(p, name) == "off" {
			continue
		}
		if viewAudience(p, name) == "members" && !caller.Member && !caller.Owner {
			return nil, errors.New("auth-required: view is for members")
		}
		v, err := s.View(ctx, name, caller, now)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if p.Views["presence"] != "off" {
		e, err := s.Presence(ctx, caller, now)
		if err != nil {
			return nil, err
		}
		out = append(out, View{Name: "presence", Event: e, Rows: maxInt(0, len(e.Tags)-1)})
	}
	return out, nil
}

// View folds and signs one named view after applying that view's audience and
// read policy. Callers serving /view/<name> should use this instead of Views,
// since the aggregate management listing is not an HTTP view endpoint.
func (s *Service) View(ctx context.Context, name string, caller policy.Access, now int64) (View, error) {
	p := s.policy()
	if !containsView(name) || effectiveViewTrigger(p, name) == "off" {
		return View{}, errors.New("not found: view")
	}
	if p.Reads == "members" && !caller.Member && !caller.Owner {
		return View{}, errors.New("restricted: views")
	}
	if p.Reads == "auth" && len(caller.PubKeys) == 0 && !caller.Owner {
		return View{}, errors.New("auth-required: views")
	}
	if viewAudience(p, name) == "members" && !caller.Member && !caller.Owner {
		return View{}, errors.New("auth-required: view is for members")
	}
	if name == "presence" {
		e, err := s.Presence(ctx, caller, now)
		if err != nil {
			return View{}, err
		}
		return View{Name: name, Event: e, Rows: maxInt(0, len(e.Tags)-1)}, nil
	}
	e, err := s.view(ctx, name, caller, now)
	if err != nil {
		return View{}, err
	}
	rows := len(e.Tags) - 3
	if name == "presence" {
		rows = len(e.Tags) - 1
	}
	return View{Name: name, Event: e, Rows: maxInt(0, rows)}, nil
}

func containsView(name string) bool {
	for _, n := range viewNames {
		if n == name {
			return true
		}
	}
	return false
}

func (s *Service) view(ctx context.Context, name string, caller policy.Access, now int64) (event.Event, error) {
	rows, err := s.store.Query(ctx, event.Filter{}, storage.QueryOptions{Now: now, Access: storage.Access{All: true}, Limit: 0})
	if err != nil {
		return event.Event{}, err
	}
	var tags [][]string
	var content string
	if name == "moderation" {
		tags, content = s.moderationFold(ctx, now)
	} else if name == "zaps" {
		tags, content = s.zapsFold(ctx, rows.Events, now)
	} else if name == "profiles" {
		people, names := s.viewPeople(ctx)
		rows.Events = onlyAuthors(rows.Events, people)
		tags, content = profileFold(rows.Events, s.relayURL, people, names)
	} else if name == "relays" {
		people, _ := s.viewPeople(ctx)
		tags, content = fold(name, onlyAuthors(rows.Events, people), s.relayURL, now)
	} else {
		tags, content = fold(name, rows.Events, s.relayURL, now)
	}
	trigger := s.policy().Views[name]
	if trigger == "" {
		trigger = map[string]string{"profiles": "daily", "relays": "daily", "calendar": "hourly", "moderation": "daily", "articles": "write", "zaps": "hourly"}[name]
	}
	tags = append([][]string{{"-"}, {"d", "bind.ws/view/" + name}, {"trigger", trigger}}, tags...)
	if viewStored(s.policy(), name) {
		return s.signed(ctx, event.KIND_VIEW, tags, content, now)
	}
	return s.signedOnly(ctx, event.KIND_VIEW, tags, content, now)
}

func onlyAuthors(events []event.Event, allowed map[string]string) []event.Event {
	out := make([]event.Event, 0, len(events))
	for _, e := range events {
		if _, ok := allowed[e.PubKey]; ok {
			out = append(out, e)
		}
	}
	return out
}

// viewPeople returns owner first, followed by community members. The SQL
// table is intentionally read defensively because records can initialize
// before the community schema during a migration.
func (s *Service) viewPeople(ctx context.Context) (map[string]string, []string) {
	p := s.policy()
	people := make(map[string]string)
	order := make([]string, 0, 8)
	if p.Owner != "" {
		people[p.Owner] = ""
		order = append(order, p.Owner)
	}
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,name FROM community_members ORDER BY pubkey`)
	if err != nil {
		return people, order
	}
	defer rows.Close()
	for rows.Next() {
		var pk, name string
		if rows.Scan(&pk, &name) != nil || pk == "" {
			continue
		}
		if _, exists := people[pk]; !exists {
			order = append(order, pk)
		}
		people[pk] = name
	}
	return people, order
}

func profileFold(events []event.Event, relayURL string, people map[string]string, order []string) ([][]string, string) {
	latest := make(map[string]event.Event)
	for _, e := range events {
		if e.Kind != event.KIND_PROFILE {
			continue
		}
		if old, ok := latest[e.PubKey]; !ok || e.CreatedAt > old.CreatedAt {
			latest[e.PubKey] = e
		}
	}
	host := relayURL
	if u, err := url.Parse(relayURL); err == nil && u.Host != "" {
		host = u.Host
	}
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "wss://"), "ws://"), "/")
	if host == "" {
		host = "relay"
	}
	out := make([][]string, 0, len(order))
	for _, pk := range order {
		m := map[string]any{}
		if e, ok := latest[pk]; ok {
			_ = json.Unmarshal([]byte(e.Content), &m)
		}
		nip05 := trimString(m["nip05"], 200)
		if people[pk] != "" {
			nip05 = people[pk] + "@" + host
		}
		out = append(out, []string{"p", pk, trimString(m["name"], 200), trimString(m["picture"], 2000), nip05})
	}
	return out, ""
}

func (s *Service) zapsFold(ctx context.Context, events []event.Event, now int64) ([][]string, string) {
	allowed := map[string]bool{}
	if owner := s.policy().Owner; owner != "" {
		allowed[owner] = true
	}
	if rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey FROM community_members`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var pk string
			if rows.Scan(&pk) == nil {
				allowed[pk] = true
			}
		}
	}
	byEvent, byAuthor := map[string]int64{}, map[string]int64{}
	for _, receipt := range events {
		if receipt.Kind != 9735 {
			continue
		}
		var req event.Event
		if json.Unmarshal([]byte(event.Tag(receipt, "description")), &req) != nil || req.Kind != 9734 {
			continue
		}
		msats := bolt11MSats(event.Tag(receipt, "bolt11"))
		if msats <= 0 {
			continue
		}
		if pk := event.Tag(req, "p"); allowed[pk] {
			byAuthor[pk] += msats
		}
		if id := event.Tag(req, "e"); len(id) == 64 {
			var exists int
			if s.store.DB().QueryRowContext(ctx, `SELECT 1 FROM events WHERE id=?`, id).Scan(&exists) == nil {
				byEvent[id] += msats
			}
		}
	}
	out := make([][]string, 0, len(byEvent)+len(byAuthor))
	for id, n := range byEvent {
		out = append(out, []string{"e", id, strconv.FormatInt(n, 10)})
	}
	for pk, n := range byAuthor {
		out = append(out, []string{"p", pk, strconv.FormatInt(n, 10)})
	}
	sort.Slice(out, func(i, j int) bool {
		ni, _ := strconv.ParseInt(out[i][2], 10, 64)
		nj, _ := strconv.ParseInt(out[j][2], 10, 64)
		if ni != nj {
			return ni > nj
		}
		return out[i][1] < out[j][1]
	})
	return out, ""
}

func (s *Service) moderationFold(ctx context.Context, now int64) ([][]string, string) {
	month := time.Unix(now, 0).UTC().Format("2006-01")
	start := time.Unix(now, 0).UTC()
	start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	one := func(table, where string) int {
		var n int
		err := s.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE "+where, start.Unix()).Scan(&n)
		if err != nil {
			return 0
		}
		return n
	}
	counts := map[string]any{"month": month, "bans": one("community_bans", "at>=?"), "reports": one("community_reports", "at>=?"), "resolved": one("community_reports", "resolved_at>=?"), "hidden": one("community_event_bans", "at>=?"), "deleted": 0, "blocked_addresses": one("community_ip_blocks", "at>=?")}
	b, _ := json.Marshal(counts)
	return [][]string{{"month", month}}, string(b)
}

func fold(name string, events []event.Event, relayURL string, now int64) ([][]string, string) {
	profiles := map[string]event.Event{}
	relays := map[string]event.Event{}
	for _, e := range events {
		if e.Kind == event.KIND_PROFILE {
			if _, ok := profiles[e.PubKey]; !ok {
				profiles[e.PubKey] = e
			}
		}
		if e.Kind == 10002 {
			if _, ok := relays[e.PubKey]; !ok {
				relays[e.PubKey] = e
			}
		}
	}
	switch name {
	case "profiles":
		out := [][]string{}
		for pk, e := range profiles {
			if e.Kind != event.KIND_PROFILE {
				continue
			}
			var m map[string]any
			_ = json.Unmarshal([]byte(e.Content), &m)
			out = append(out, []string{"p", pk, trimString(m["name"], 200), trimString(m["picture"], 200), trimString(m["nip05"], 200)})
		}
		sort.Slice(out, func(i, j int) bool { return out[i][1] < out[j][1] })
		return out, ""
	case "relays":
		counts := map[string]int{}
		for _, e := range relays {
			seen := map[string]bool{}
			for _, t := range e.Tags {
				if len(t) < 2 || t[0] != "r" {
					continue
				}
				x, err := url.Parse(strings.TrimSpace(t[1]))
				if err != nil || (x.Scheme != "ws" && x.Scheme != "wss") || x.Host == "" {
					continue
				}
				u := strings.ToLower(x.Scheme + "://" + x.Host + strings.TrimRight(x.EscapedPath(), "/"))
				if x.RawQuery != "" {
					u += "?" + x.RawQuery
				}
				if seen[u] {
					continue
				}
				seen[u] = true
				counts[u]++
			}
		}
		keys := make([]string, 0, len(counts))
		for u := range counts {
			keys = append(keys, u)
		}
		sort.Slice(keys, func(i, j int) bool {
			if counts[keys[i]] != counts[keys[j]] {
				return counts[keys[i]] > counts[keys[j]]
			}
			return keys[i] < keys[j]
		})
		out := [][]string{}
		for _, u := range keys[:minInt(len(keys), 100)] {
			out = append(out, []string{"r", u, strconv.Itoa(counts[u])})
		}
		return out, ""
	case "calendar":
		type cal struct {
			e  event.Event
			at int64
		}
		var upcoming []cal
		for _, e := range events {
			if e.Kind != 31922 && e.Kind != 31923 {
				continue
			}
			at := calendarStart(e)
			if at < now-86400 || at > now+30*86400 {
				continue
			}
			upcoming = append(upcoming, cal{e, at})
		}
		sort.Slice(upcoming, func(i, j int) bool { return upcoming[i].at < upcoming[j].at })
		rsvp := map[string]map[string]bool{}
		for _, e := range events {
			if e.Kind != 31925 || event.Tag(e, "status") != "accepted" {
				continue
			}
			for _, a := range event.TagValues(e, "a") {
				if rsvp[a] == nil {
					rsvp[a] = map[string]bool{}
				}
				rsvp[a][e.PubKey] = true
			}
		}
		out := [][]string{}
		for _, x := range upcoming[:minInt(len(upcoming), 50)] {
			a := strconv.Itoa(x.e.Kind) + ":" + x.e.PubKey + ":" + event.Tag(x.e, "d")
			row := []string{"a", a, strconv.FormatInt(x.at, 10), event.Tag(x.e, "title")}
			if n := len(rsvp[a]); n > 0 {
				row = append(row, strconv.Itoa(n))
			}
			out = append(out, row)
		}
		return out, ""
	case "moderation":
		counts := map[string]int{"reports": 0, "deleted": 0}
		for _, e := range events {
			if e.Kind == event.KIND_REPORT {
				counts["reports"]++
			}
			if e.Kind == event.KIND_DELETION {
				counts["deleted"]++
			}
		}
		b, _ := json.Marshal(map[string]any{"month": time.Unix(now, 0).UTC().Format("2006-01"), "reports": counts["reports"], "deleted": counts["deleted"]})
		return [][]string{{"month", time.Unix(now, 0).UTC().Format("2006-01")}}, string(b)
	case "articles":
		articles := make([]event.Event, 0)
		for _, e := range events {
			if e.Kind == 30023 {
				articles = append(articles, e)
			}
		}
		sort.SliceStable(articles, func(i, j int) bool { return articlePublished(articles[i]) > articlePublished(articles[j]) })
		out := [][]string{}
		for _, e := range articles[:minInt(len(articles), 100)] {
			published := event.Tag(e, "published_at")
			if published == "" {
				published = strconv.FormatInt(e.CreatedAt, 10)
			}
			out = append(out, []string{"a", strconv.Itoa(e.Kind) + ":" + e.PubKey + ":" + event.Tag(e, "d"), event.Tag(e, "title"), published})
		}
		return out, ""
	case "zaps":
		byEvent, byAuthor := map[string]int64{}, map[string]int64{}
		for _, receipt := range events {
			if receipt.Kind != 9735 {
				continue
			}
			var req event.Event
			if json.Unmarshal([]byte(event.Tag(receipt, "description")), &req) != nil || req.Kind != 9734 {
				continue
			}
			msats := bolt11MSats(event.Tag(receipt, "bolt11"))
			if msats <= 0 {
				continue
			}
			if target := event.Tag(req, "p"); target != "" {
				byAuthor[target] += msats
			}
			if target := event.Tag(req, "e"); len(target) == 64 {
				byEvent[target] += msats
			}
		}
		out := make([][]string, 0, len(byEvent)+len(byAuthor))
		for id, n := range byEvent {
			out = append(out, []string{"e", id, strconv.FormatInt(n, 10)})
		}
		for pk, n := range byAuthor {
			out = append(out, []string{"p", pk, strconv.FormatInt(n, 10)})
		}
		sort.Slice(out, func(i, j int) bool {
			ni, _ := strconv.ParseInt(out[i][2], 10, 64)
			nj, _ := strconv.ParseInt(out[j][2], 10, 64)
			if ni != nj {
				return ni > nj
			}
			return out[i][1] < out[j][1]
		})
		return out, ""
	case "presence":
		return nil, ""
	default:
		return nil, ""
	}
}

func calendarStart(e event.Event) int64 {
	s := event.Tag(e, "start")
	if e.Kind == 31922 {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return 0
		}
		return t.Unix()
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
func articlePublished(e event.Event) int64 {
	s := event.Tag(e, "published_at")
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return e.CreatedAt
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func trimString(v any, n int) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}

// bolt11MSats parses the amount prefix of a BOLT11 invoice. It intentionally
// ignores the signature and tags; the receipt signature and description are
// validated by the event ingest path before a view sees them.
func bolt11MSats(invoice string) int64 {
	if len(invoice) < 6 || !strings.HasPrefix(strings.ToLower(invoice), "lnbc") {
		return 0
	}
	s := strings.ToLower(invoice[4:])
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0
	}
	amount := s[:i]
	suffix := ""
	if i < len(s) {
		suffix = s[i : i+1]
	}
	var factor float64
	switch suffix {
	case "m":
		factor = 1e8
	case "u":
		factor = 1e5
	case "n":
		factor = 1e2
	case "p":
		factor = 0.1
	case "":
		factor = 1e11
	default:
		return 0
	}
	var value float64
	if _, err := fmt.Sscanf(amount, "%f", &value); err != nil {
		return 0
	}
	value *= factor
	if value <= 0 || value > math.MaxInt64 {
		return 0
	}
	return int64(value + 0.5)
}

// MarkView durably marks a write-triggered view dirty. Tick publishes one
// coalesced fold after the short source-compatible debounce window.
func (s *Service) MarkView(ctx context.Context, name string, now int64) error {
	p := s.policy()
	if name == "presence" || effectiveViewTrigger(p, name) != "write" || !viewStored(p, name) {
		return nil
	}
	var dirty int64
	if err := s.store.GetSetting(ctx, "records.view."+name+".dirty", &dirty); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if dirty == 0 {
		return s.store.PutSetting(ctx, "records.view."+name+".dirty", now)
	}
	return nil
}

// NextDue returns the earliest scheduled view or pending write-coalescing
// deadline, allowing the host scheduler to wake without an in-memory timer.
func (s *Service) NextDue(ctx context.Context, now int64) (int64, error) {
	p := s.policy()
	next := int64(0)
	periods := map[string]int64{"profiles": 86400, "relays": 86400, "calendar": 3600, "moderation": 86400, "articles": 86400, "zaps": 3600}
	for name, period := range periods {
		mode := effectiveViewTrigger(p, name)
		if mode == "off" || !viewStored(p, name) {
			continue
		}
		if mode == "write" {
			period = 86400
		}
		if mode == "hourly" {
			period = 3600
		}
		if mode == "daily" {
			period = 86400
		}
		if mode == "write" {
			var dirty int64
			if err := s.store.GetSetting(ctx, "records.view."+name+".dirty", &dirty); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			if dirty != 0 {
				due := dirty + 10
				if due <= now {
					return now, nil
				}
				if next == 0 || due < next {
					next = due
				}
				continue
			}
		}
		var last int64
		if err := s.store.GetSetting(ctx, "records.view."+name+".at", &last); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		due := last + period
		if last == 0 || due <= now {
			return now, nil
		}
		if next == 0 || due < next {
			next = due
		}
	}
	return next, nil
}

// Tick is called by the host scheduler; it does not start a goroutine.
func (s *Service) Tick(ctx context.Context, now int64) error {
	p := s.policy()
	if err := s.tickViews(ctx, p, now); err != nil {
		return err
	}
	if p.Succession == nil || p.Succession.Heir == "" || p.Owner == "" {
		return nil
	}
	// The heir must remain a member while the plan is armed.
	var member, memberCount int
	err := s.store.DB().QueryRowContext(ctx, `SELECT 1 FROM community_members WHERE pubkey=? LIMIT 1`, p.Succession.Heir).Scan(&member)
	if err == nil {
		_ = s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM community_members`).Scan(&memberCount)
	}
	if err != nil && !strings.Contains(err.Error(), "no such table") && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if memberCount > 0 && (errors.Is(err, sql.ErrNoRows) || (err == nil && member == 0)) {
		if s.setPolicy != nil {
			next := p
			next.Succession = nil
			if err := s.setPolicy(next); err != nil {
				return err
			}
		}
		_ = s.store.PutSetting(ctx, "records.succession_warning", int64(0))
		_ = s.store.PutSetting(ctx, "records.succession_notify", int64(0))
		return s.notifyText(ctx, "succession", "Your heir is no longer a member, so the handover plan is off. Name another heir if you still want one.", "succession on relay", p.Owner, now)
	}
	var heartbeat int64
	if err := s.store.GetSetting(ctx, "records.owner_seen", &heartbeat); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if heartbeat == 0 {
		heartbeat = now
		if err := s.store.PutSetting(ctx, "records.owner_seen", heartbeat); err != nil {
			return err
		}
		return nil
	}
	delay := int64(p.Succession.AfterDays) * 86400
	if now < heartbeat+delay {
		return nil
	}
	var warned int64
	_ = s.store.GetSetting(ctx, "records.succession_warning", &warned)
	if warned == 0 {
		if err := s.store.PutSetting(ctx, "records.succession_warning", now); err != nil {
			return err
		}
		if err := s.store.PutSetting(ctx, "records.succession_notify", now); err != nil {
			return err
		}
		return s.notifyText(ctx, "succession", "You have been away long enough to start the succession warning period. Any signed action cancels it.", "succession on relay", p.Owner, now)
	}
	warnUntil := warned + 30*86400
	if now < warnUntil {
		var last int64
		_ = s.store.GetSetting(ctx, "records.succession_notify", &last)
		if now-last >= 7*86400 {
			_ = s.store.PutSetting(ctx, "records.succession_notify", now)
			return s.notifyText(ctx, "succession", "Still no sign of you. The relay will go to your heir unless you sign in.", "succession on relay", p.Owner, now)
		}
		return nil
	}
	old, heir := p.Owner, p.Succession.Heir
	if s.setPolicy == nil {
		return errors.New("records: succession policy writer unavailable")
	}
	next := p
	next.Owner = heir
	next.Succession = nil
	if err := s.setPolicy(next); err != nil {
		return err
	}
	if err := appendSuccessionLog(ctx, s.store, successionLog{At: now, From: old, To: heir}); err != nil {
		return err
	}
	_ = s.store.PutSetting(ctx, "records.owner_seen", now)
	_ = s.store.PutSetting(ctx, "records.succession_warning", int64(0))
	_ = s.store.PutSetting(ctx, "records.succession_notify", int64(0))
	if s.onTransfer != nil {
		if err := s.onTransfer(ctx, old, heir); err != nil {
			return err
		}
	}
	if err := s.notifyText(ctx, "succession", "The relay now belongs to your heir, as you planned. You stay on as a moderator.", "succession on relay", old, now); err != nil {
		return err
	}
	return s.notifyText(ctx, "succession", "The relay is yours now. Its owner named you heir and has been away through the warning period.", "succession on relay", heir, now)
}

type successionLog struct {
	At   int64  `json:"at"`
	From string `json:"from"`
	To   string `json:"to"`
}

func appendSuccessionLog(ctx context.Context, st *storage.Store, entry successionLog) error {
	var log []successionLog
	_ = st.GetSetting(ctx, "records.succession_log", &log)
	log = append(log, entry)
	if len(log) > 10 {
		log = log[len(log)-10:]
	}
	return st.PutSetting(ctx, "records.succession_log", log)
}

func (s *Service) tickViews(ctx context.Context, p policy.Policy, now int64) error {
	defaults := map[string]int64{"profiles": 86400, "relays": 86400, "calendar": 3600, "moderation": 86400, "articles": 86400, "zaps": 3600}
	for name, period := range defaults {
		mode := effectiveViewTrigger(p, name)
		if mode == "off" || !viewStored(p, name) {
			continue
		}
		if mode == "daily" {
			period = 86400
		}
		if mode == "hourly" {
			period = 3600
		}
		key := "records.view." + name + ".at"
		var last int64
		if err := s.store.GetSetting(ctx, key, &last); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var dirty int64
		if mode == "write" {
			if err := s.store.GetSetting(ctx, "records.view."+name+".dirty", &dirty); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if dirty != 0 && now-dirty >= 10 {
				// A write burst is coalesced into one publication.
			} else if dirty != 0 {
				continue
			} else if last != 0 && now-last < period-300 {
				continue
			}
		} else if last != 0 && now-last < period-300 {
			continue
		}
		if mode == "hourly" {
			fp, err := s.viewFingerprint(ctx, name, now)
			if err != nil {
				return err
			}
			var previous string
			if err := s.store.GetSetting(ctx, "records.view."+name+".fingerprint", &previous); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if previous != "" && previous == fp {
				if err := s.store.PutSetting(ctx, key, now); err != nil {
					return err
				}
				continue
			}
			if err := s.store.PutSetting(ctx, "records.view."+name+".fingerprint", fp); err != nil {
				return err
			}
		}
		e, err := s.view(ctx, name, policy.Access{Owner: true}, now)
		if err != nil {
			return err
		}
		if err := s.store.PutSetting(ctx, key, now); err != nil {
			return err
		}
		if err := s.recordViewRun(ctx, name, now, maxInt(0, len(e.Tags)-3)); err != nil {
			return err
		}
		if mode == "write" && dirty != 0 {
			if err := s.store.PutSetting(ctx, "records.view."+name+".dirty", int64(0)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) viewFingerprint(ctx context.Context, name string, now int64) (string, error) {
	var n, max sql.NullInt64
	var q string
	switch name {
	case "calendar":
		q = `SELECT count(*),max(created_at) FROM events WHERE kind IN (31922,31923,31925)`
	case "zaps":
		q = `SELECT count(*),max(created_at) FROM events WHERE kind=9735`
	default:
		return "", nil
	}
	if err := s.store.DB().QueryRowContext(ctx, q).Scan(&n, &max); err != nil {
		return "", err
	}
	day := int64(0)
	if name == "calendar" {
		day = now / 86400
	}
	return fmt.Sprintf("%d:%d:%d", n.Int64, max.Int64, day), nil
}

func (s *Service) recordViewRun(ctx context.Context, name string, at int64, rows int) error {
	if _, err := s.store.DB().ExecContext(ctx, `INSERT INTO records_view_runs(name,at,rows) VALUES(?,?,?)`, name, at, rows); err != nil {
		return err
	}
	_, err := s.store.DB().ExecContext(ctx, `DELETE FROM records_view_runs WHERE name=? AND rowid NOT IN (SELECT rowid FROM records_view_runs WHERE name=? ORDER BY at DESC,rowid DESC LIMIT 60)`, name, name)
	return err
}

// ViewRuns returns the bounded history used by management views and digests.
// Runs are newest-last, matching the source relay contract.
func (s *Service) ViewRuns(ctx context.Context, name string) ([]ViewRun, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT at,rows FROM records_view_runs WHERE name=? ORDER BY at,rowid`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ViewRun, 0, 60)
	for rows.Next() {
		var r ViewRun
		if err := rows.Scan(&r.At, &r.Rows); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) ViewRowsSince(ctx context.Context, name string, since int64) (int64, error) {
	var total sql.NullInt64
	err := s.store.DB().QueryRowContext(ctx, `SELECT sum(rows) FROM records_view_runs WHERE name=? AND at>=?`, name, since).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Service) Heartbeat(ctx context.Context, owner string, now int64) error {
	if owner == "" || owner != s.policy().Owner {
		return errors.New("restricted: owner heartbeat")
	}
	if err := s.store.PutSetting(ctx, "records.owner_seen", now); err != nil {
		return err
	}
	_ = s.store.PutSetting(ctx, "records.succession_warning", int64(0))
	return s.store.PutSetting(ctx, "records.succession_notify", int64(0))
}

func (s *Service) notify(ctx context.Context, typ, owner, target string, now int64) error {
	if !s.policy().Notify.Succession && (typ == "succession" || typ == "succession-transfer") {
		return nil
	}
	content, _ := json.Marshal(map[string]string{"type": typ, "target": target})
	wrapped, err := s.giftWrap(owner, string(content), "relay", now)
	if err != nil {
		return err
	}
	if s.deliverNotification != nil {
		if err := s.deliverNotification(ctx, wrapped, target); err != nil {
			return err
		}
		return s.push(ctx, target, typ, "relay", "")
	}
	if s.onGenerated != nil {
		return s.onGenerated(ctx, wrapped)
	}
	return err
}

func (s *Service) push(ctx context.Context, recipient, kind, subject, text string) error {
	if s.pushNotification == nil {
		return nil
	}
	return s.pushNotification(ctx, recipient, kind, subject, text)
}

// Notify applies the source notification preferences and emits a durable
// gift-wrap through OnGenerated. The host callback is responsible for saving
// the event and enqueueing remote delivery in the same transaction.
func (s *Service) Notify(ctx context.Context, kind, text, subject, target string, now int64) error {
	p := s.policy()
	if kind != "test" {
		enabled := map[string]bool{"reports": p.Notify.Reports, "jobs": p.Notify.Jobs, "succession": p.Notify.Succession, "digest": p.Notify.Digest}[kind]
		if !enabled {
			return nil
		}
	}
	if target == "" {
		target = p.Owner
	}
	return s.notifyText(ctx, kind, text, subject, target, now)
}

func (s *Service) notifyText(ctx context.Context, kind, text, subject, target string, now int64) error {
	if target == "" {
		return errors.New("notify: empty recipient")
	}
	if s.deliverNotification == nil && s.onGenerated == nil {
		return nil
	}
	wrapped, err := s.giftWrap(target, text, subject, now)
	if err != nil {
		return err
	}
	if s.deliverNotification != nil {
		if err := s.deliverNotification(ctx, wrapped, target); err != nil {
			return err
		}
		return s.push(ctx, target, kind, subject, text)
	}
	if s.onGenerated != nil {
		return s.onGenerated(ctx, wrapped)
	}
	return nil
}

// giftWrap creates the NIP-17 kind-14 rumor, NIP-59 seal and gift wrap using
// the relay identity as sender. Only the resulting kind-1059 event leaves this
// package; the relay secret never appears in a record or API response.
func (s *Service) giftWrap(recipient, content, subject string, now int64) (event.Event, error) {
	rpk, err := nostr.PubKeyFromHex(recipient)
	if err != nil {
		return event.Event{}, err
	}
	sk, err := nostr.SecretKeyFromHex(s.secret)
	if err != nil {
		return event.Event{}, err
	}
	// Build the three NIP-59 records with the project's canonical event signer.
	// This avoids the upstream nostr JSON encoder's unsafe fast path under -race.
	tags := [][]string{{"p", recipient}}
	if subject != "" {
		tags = append(tags, []string{"subject", subject})
	}
	rumor := event.Event{CreatedAt: now, Kind: event.KIND_DM, Tags: tags, Content: content}
	if err := event.Sign(&rumor, s.secret); err != nil {
		return event.Event{}, err
	}
	rumor.Sig = strings.Repeat("0", 128)
	rumorRaw, err := json.Marshal(rumor)
	if err != nil {
		return event.Event{}, err
	}
	key, err := nip44.GenerateConversationKey(rpk, sk)
	if err != nil {
		return event.Event{}, err
	}
	rumorCipher, err := nip44.Encrypt(string(rumorRaw), key)
	if err != nil {
		return event.Event{}, err
	}
	seal := event.Event{CreatedAt: now, Kind: 13, Tags: [][]string{}, Content: rumorCipher}
	if err := event.Sign(&seal, s.secret); err != nil {
		return event.Event{}, err
	}
	nonce := nostr.Generate()
	outerKey, err := nip44.GenerateConversationKey(rpk, nonce)
	if err != nil {
		return event.Event{}, err
	}
	sealRaw, err := json.Marshal(seal)
	if err != nil {
		return event.Event{}, err
	}
	outerCipher, err := nip44.Encrypt(string(sealRaw), outerKey)
	if err != nil {
		return event.Event{}, err
	}
	wrapped := event.Event{CreatedAt: now, Kind: event.KIND_WRAP, Tags: [][]string{{"p", recipient}}, Content: outerCipher}
	if err := event.Sign(&wrapped, nonce.Hex()); err != nil {
		return event.Event{}, err
	}
	return wrapped, nil
}

// Execute covers record-owned management operations. Policy/community/storage
// operations remain in their owning services; unknown methods are explicit.
func (s *Service) Execute(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
	p := s.policy()
	if actor != "" && actor != p.Owner {
		return nil, errors.New("restricted: owner record operation")
	}
	now := time.Now().Unix()
	_ = s.audit(ctx, actor, method, "", "", now)
	switch method {
	case "successionstatus":
		var seen, warning, last int64
		_ = s.store.GetSetting(ctx, "records.owner_seen", &seen)
		_ = s.store.GetSetting(ctx, "records.succession_warning", &warning)
		_ = s.store.GetSetting(ctx, "records.succession_notify", &last)
		var log []successionLog
		_ = s.store.GetSetting(ctx, "records.succession_log", &log)
		var warningState any
		if warning > 0 {
			warningState = map[string]any{"since": warning, "lastNotified": last}
		}
		return map[string]any{"ownerSeenAt": seen, "warning": warningState, "handoverAt": func() int64 {
			if warning > 0 {
				return warning + 30*86400
			}
			if p.Succession != nil {
				return seen + int64(p.Succession.AfterDays)*86400 + 30*86400
			}
			return 0
		}(), "log": log, "succession": p.Succession}, nil
	case "setsuccession":
		if len(params) == 0 {
			return nil, errors.New("invalid: missing succession")
		}
		var v policy.Succession
		if err := json.Unmarshal(params[0], &v); err != nil {
			return nil, err
		}
		if v.AfterDays != 90 && v.AfterDays != 180 && v.AfterDays != 365 {
			return nil, errors.New("invalid: succession delay")
		}
		if s.setPolicy != nil {
			next := p
			next.Succession = &v
			next.Notify.Succession = true
			if err := s.setPolicy(next); err != nil {
				return nil, err
			}
		}
		return v, s.store.PutSetting(ctx, "records.succession", v)
	case "clearsuccession":
		if s.setPolicy != nil {
			next := p
			next.Succession = nil
			next.Notify.Succession = false
			if err := s.setPolicy(next); err != nil {
				return nil, err
			}
		}
		return true, s.store.PutSetting(ctx, "records.succession", nil)
	case "listpins":
		return s.store.DB().QueryContext(ctx, `SELECT ref FROM records_pins ORDER BY position`)
	case "pinevent", "unpinevent":
		if len(params) == 0 {
			return nil, errors.New("invalid: missing pin")
		}
		var ref string
		if err := json.Unmarshal(params[0], &ref); err != nil {
			return nil, err
		}
		var refs []string
		rows, _ := s.store.DB().QueryContext(ctx, `SELECT ref FROM records_pins ORDER BY position`)
		for rows.Next() {
			var x string
			_ = rows.Scan(&x)
			if x != ref {
				refs = append(refs, x)
			}
		}
		_ = rows.Close()
		if method == "pinevent" {
			refs = append(refs, ref)
		}
		return s.SetPins(ctx, refs, now)
	case "listviews":
		return s.ViewSummaries(ctx)
	case "notifytest":
		return true, s.notify(ctx, "test", p.Owner, p.Owner, now)
	case "notifystatus":
		return map[string]any{"reports": p.Notify.Reports, "jobs": p.Notify.Jobs, "succession": p.Notify.Succession, "digest": p.Notify.Digest}, nil
	default:
		return nil, fmt.Errorf("unsupported: records method %q", method)
	}
}

// ViewSummaries is the management/NIP-86 listing. It reports state and run
// history without folding or signing any view, so a mixed-audience relay can
// expose its catalogue without accidentally materializing private data.
func (s *Service) ViewSummaries(ctx context.Context) ([]map[string]any, error) {
	p := s.policy()
	out := make([]map[string]any, 0, len(viewNames))
	for _, name := range viewNames {
		trigger := effectiveViewTrigger(p, name)
		runs, err := s.ViewRuns(ctx, name)
		if err != nil {
			return nil, err
		}
		var last any
		if len(runs) > 0 {
			last = runs[len(runs)-1]
		}
		choices := []string{"off", "write", "hourly", "daily"}
		if name == "presence" {
			choices = []string{"off"}
		}
		about := map[string]string{"profiles": "every member's newest profile in one record", "relays": "where the members are: the union of their relay lists", "calendar": "what is on: calendar events starting in the next 30 days, with RSVPs counted", "moderation": "this month's moderation counts, no ids", "articles": "the newest hundred articles, by address", "zaps": "zap totals for the top notes and authors here", "presence": "who is connected now and who wrote in the last 15 minutes"}[name]
		out = append(out, map[string]any{
			"name": name, "about": about, "trigger": trigger, "default": defaultViewTrigger(name), "choices": choices,
			"audience": viewAudience(p, name), "on": trigger != "off", "stored": viewStored(p, name),
			"last": last, "path": "/view/" + name,
		})
	}
	return out, nil
}

func (s *Service) ViewNames() []string { return append([]string(nil), viewNames...) }

func (s *Service) Clock(ctx context.Context) (int64, error) {
	var v int64
	err := s.store.GetSetting(ctx, "records.clock", &v)
	return v, err
}

func parseIntSetting(ctx context.Context, s *storage.Store, key string) int64 {
	var v int64
	_ = s.GetSetting(ctx, key, &v)
	return v
}

var _ = strconv.IntSize
