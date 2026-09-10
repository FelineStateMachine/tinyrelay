// Package configport imports, exports, plans and applies bind.ws relay files.
package configport

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/templates"
)

const Format = "bind.ws/relay-config/2"

type Member struct {
	PubKey string `json:"pubkey"`
	Name   string `json:"name,omitempty"`
	Note   string `json:"note,omitempty"`
	Role   string `json:"role"`
}
type Rule struct {
	Kind *int `json:"kind"`
	Days int  `json:"days"`
}
type Config struct {
	Format       string                     `json:"format"`
	ExportedAt   int64                      `json:"exported_at,omitempty"`
	Name         string                     `json:"name,omitempty"`
	Template     *templates.Template        `json:"template,omitempty"`
	Policy       map[string]json.RawMessage `json:"policy,omitempty"`
	Members      []Member                   `json:"members,omitempty"`
	Bans         []map[string]any           `json:"bans,omitempty"`
	Addresses    []map[string]any           `json:"addresses,omitempty"`
	BannedEvents []map[string]any           `json:"banned_events,omitempty"`
	Kinds        struct {
		Allow []int `json:"allow"`
		Block []int `json:"block"`
	} `json:"kinds,omitempty"`
	Retention   []Rule           `json:"retention,omitempty"`
	Connections []map[string]any `json:"connections,omitempty"`
	Jobs        []map[string]any `json:"jobs,omitempty"`
	Sections    []string         `json:"-"`
	Warnings    []string         `json:"-"`
}
type Changes struct {
	Policy         []string      `json:"policy,omitempty"`
	Members        []string      `json:"members,omitempty"`
	MembersCleared bool          `json:"membersCleared,omitempty"`
	Warnings       []string      `json:"warnings,omitempty"`
	FinalPolicy    policy.Policy `json:"-"`
}
type ConfigStore struct {
	Store     *storage.Store
	Community *community.Service
	Policy    func() policy.Policy
	// OnApplied runs only after the durable transaction commits. It is the
	// integration point for replacing an in-memory policy and closing stale
	// subscriptions.
	OnApplied func(policy.Policy)
	// ValidatePolicy runs before a config transaction is committed.
	ValidatePolicy func(policy.Policy) error
}

type ApplyOptions struct {
	DryRun         bool
	MigrationOwner string
}

func New(c ConfigStore) *ConfigStore {
	if c.Store != nil {
		_, _ = c.Store.DB().Exec(`CREATE TABLE IF NOT EXISTS config_connections(position INTEGER PRIMARY KEY, value TEXT NOT NULL)`)
		_, _ = c.Store.DB().Exec(`CREATE TABLE IF NOT EXISTS config_jobs(id TEXT PRIMARY KEY, recipe TEXT NOT NULL)`)
		_ = replication.EnsureJobSchema(c.Store.DB())
	}
	return &c
}

func Parse(raw []byte) (Config, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Config{}, fmt.Errorf("invalid: config JSON: %w", err)
	}
	var f string
	_ = json.Unmarshal(obj["format"], &f)
	if f != "bind.ws/relay-config/1" && f != Format {
		return Config{}, errors.New("invalid: format must be bind.ws/relay-config/2")
	}
	var c Config
	c.Format = Format
	_ = json.Unmarshal(obj["name"], &c.Name)
	if b := obj["exported_at"]; b != nil {
		_ = json.Unmarshal(b, &c.ExportedAt)
	}
	for key, value := range obj {
		switch key {
		case "format", "name", "exported_at":
		case "policy":
			if err := json.Unmarshal(value, &c.Policy); err != nil {
				return Config{}, errors.New("invalid: policy must be an object")
			}
			c.Sections = append(c.Sections, "policy")
			warnPolicy(&c)
		case "members":
			if err := json.Unmarshal(value, &c.Members); err != nil {
				return Config{}, errors.New("invalid: members must be a list")
			}
			for _, member := range c.Members {
				if len(member.PubKey) != 64 {
					return Config{}, errors.New("invalid: member pubkey must be 64 hex chars")
				}
			}
			c.Sections = append(c.Sections, "members")
		case "bans":
			_ = json.Unmarshal(value, &c.Bans)
			c.Sections = append(c.Sections, "bans")
		case "addresses":
			_ = json.Unmarshal(value, &c.Addresses)
			c.Sections = append(c.Sections, "addresses")
		case "banned_events":
			_ = json.Unmarshal(value, &c.BannedEvents)
			c.Sections = append(c.Sections, "banned_events")
		case "kinds":
			if err := json.Unmarshal(value, &c.Kinds); err != nil {
				return Config{}, err
			}
			c.Sections = append(c.Sections, "kinds")
		case "retention":
			if err := json.Unmarshal(value, &c.Retention); err != nil {
				return Config{}, err
			}
			c.Sections = append(c.Sections, "retention")
		case "connections":
			if err := json.Unmarshal(value, &c.Connections); err != nil {
				return Config{}, err
			}
			c.Sections = append(c.Sections, "connections")
		case "jobs":
			if err := json.Unmarshal(value, &c.Jobs); err != nil {
				return Config{}, errors.New("invalid: jobs must be a list")
			}
			c.Sections = append(c.Sections, "jobs")
		default:
			c.Warnings = append(c.Warnings, key+": not a setting")
		}
	}
	sort.Strings(c.Sections)
	return c, nil
}

func warnPolicy(c *Config) {
	for key := range c.Policy {
		if key == "lease" {
			c.Warnings = append(c.Warnings, "policy.lease: legacy lease imported disabled; explicit owner mapping required")
		} else if key == "owner" {
			c.Warnings = append(c.Warnings, "policy.owner: never imported; tenant owner must be assigned by the operator")
		} else if key != "fileLimits" && (key == "succession" || key == "customHosts" || key == "fuel" || strings.Contains(strings.ToLower(key), "limit") || strings.HasPrefix(strings.ToLower(key), "free") || strings.HasPrefix(strings.ToLower(key), "sats")) {
			c.Warnings = append(c.Warnings, "policy."+key+": removed from self-hosted runtime")
		}
	}
}

func (s *ConfigStore) Export(ctx context.Context) (Config, error) {
	p := s.Policy()
	raw, _ := json.Marshal(p)
	var pm map[string]json.RawMessage
	_ = json.Unmarshal(raw, &pm)
	for _, key := range []string{"owner", "succession", "customHosts", "fuel", "lease"} {
		delete(pm, key)
	}
	c := Config{Format: Format, ExportedAt: time.Now().Unix(), Policy: pm, Sections: []string{"policy", "members", "bans", "addresses", "banned_events", "kinds", "retention", "connections"}, Warnings: []string{"identity private material is never exported"}}
	if s.Store == nil {
		return c, errors.New("config: missing store")
	}
	communityConfig, err := community.ExportConfig(ctx, s.Store)
	if err != nil {
		return c, err
	}
	for _, m := range communityConfig.Members {
		c.Members = append(c.Members, Member{PubKey: m.PubKey, Name: m.Name, Note: m.Note, Role: m.Role})
	}
	for _, item := range communityConfig.Bans {
		c.Bans = append(c.Bans, map[string]any{"pubkey": item.PubKey, "reason": item.Reason})
	}
	for _, item := range communityConfig.IPBlocks {
		c.Addresses = append(c.Addresses, map[string]any{"ip": item.IP, "reason": item.Reason})
	}
	for _, item := range communityConfig.EventBans {
		c.BannedEvents = append(c.BannedEvents, map[string]any{"id": item.ID, "reason": item.Reason})
	}
	for _, item := range communityConfig.KindRules {
		if item.Rule == "allow" {
			c.Kinds.Allow = append(c.Kinds.Allow, item.Kind)
		} else if item.Rule == "block" {
			c.Kinds.Block = append(c.Kinds.Block, item.Kind)
		}
	}
	for _, item := range communityConfig.Retention {
		c.Retention = append(c.Retention, Rule{Kind: item.Kind, Days: item.Days})
	}
	rows, err := s.Store.DB().QueryContext(ctx, `SELECT value FROM config_connections ORDER BY position`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return c, err
			}
			var conn map[string]any
			if json.Unmarshal([]byte(raw), &conn) == nil {
				c.Connections = append(c.Connections, conn)
			}
		}
	}
	rows, err = s.Store.DB().QueryContext(ctx, `SELECT id,recipe FROM config_jobs ORDER BY id`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, recipe string
			if err := rows.Scan(&id, &recipe); err != nil {
				return c, err
			}
			var job map[string]any
			if json.Unmarshal([]byte(recipe), &job) != nil {
				job = map[string]any{"recipe": recipe}
			}
			job["id"] = id
			c.Jobs = append(c.Jobs, job)
		}
		if err := rows.Err(); err != nil {
			return c, err
		}
		if len(c.Jobs) > 0 {
			c.Sections = append(c.Sections, "jobs")
		}
	}
	sort.Strings(c.Sections)
	return c, nil
}

func (s *ConfigStore) Plan(ctx context.Context, c Config) (Changes, error) {
	if s.Policy == nil {
		return Changes{}, errors.New("config: missing policy accessor")
	}
	cur := s.Policy()
	ch := Changes{Warnings: append([]string(nil), c.Warnings...)}
	next, err := policy.Patch(cur, c.Policy)
	if err != nil {
		return Changes{}, fmt.Errorf("config policy: %w", err)
	}
	ch.FinalPolicy = next
	for k := range c.Policy {
		if fmt.Sprint(marshalRaw(cur, k)) != fmt.Sprint(marshalRaw(next, k)) {
			ch.Policy = append(ch.Policy, k)
		}
	}
	if has(c, "members") {
		ch.MembersCleared = len(c.Members) == 0
		ch.Members = append(ch.Members, "replace members")
	}
	return ch, nil
}

func finalPolicy(cur policy.Policy, c Config, migrationOwner string) (policy.Policy, error) {
	next, err := policy.Patch(cur, c.Policy)
	if err != nil {
		return policy.Policy{}, fmt.Errorf("config policy: %w", err)
	}
	// A legacy lease is evidence only. It can never silently become ownership.
	// Existing tenants retain their explicit owner; unowned imports require an
	// operator supplied mapping.
	if _, leased := c.Policy["lease"]; leased && cur.Owner == "" {
		if migrationOwner == "" {
			return policy.Policy{}, errors.New("config migration: leased relay has no owner; supply migrationOwner")
		}
		if len(migrationOwner) != 64 {
			return policy.Policy{}, errors.New("config migration: migrationOwner must be a 64-hex pubkey")
		}
		if _, err := hex.DecodeString(migrationOwner); err != nil {
			return policy.Policy{}, errors.New("config migration: migrationOwner must be a 64-hex pubkey")
		}
		next.Owner = migrationOwner
	}
	return next, nil
}

func marshalRaw(p policy.Policy, key string) []byte {
	b, _ := json.Marshal(p)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	return m[key]
}
func has(c Config, key string) bool {
	for _, x := range c.Sections {
		if x == key {
			return true
		}
	}
	return false
}

func (s *ConfigStore) Apply(ctx context.Context, c Config, dryRun bool) (Changes, error) {
	return s.ApplyWithOptions(ctx, c, ApplyOptions{DryRun: dryRun})
}

func (s *ConfigStore) ApplyWithOptions(ctx context.Context, c Config, opts ApplyOptions) (Changes, error) {
	ch, err := s.Plan(ctx, c)
	if err != nil {
		return Changes{}, err
	}
	cur := s.Policy()
	ch.FinalPolicy, err = finalPolicy(cur, c, opts.MigrationOwner)
	if err != nil {
		return Changes{}, err
	}
	if s.ValidatePolicy != nil {
		if err := s.ValidatePolicy(ch.FinalPolicy); err != nil {
			return Changes{}, err
		}
	}
	if opts.DryRun {
		return ch, nil
	}
	if s.Store == nil {
		return Changes{}, errors.New("config: missing store")
	}
	err = s.Store.WithTx(ctx, func(tx *sql.Tx) error {
		// Persist the fully merged policy before touching metadata. All writes
		// remain in this transaction so a later validation/constraint failure
		// cannot leave the relay half-imported.
		if has(c, "policy") || (ch.FinalPolicy.Owner != "" && s.Policy().Owner == "") {
			if err := storage.PutSetting(ctx, tx, "policy", ch.FinalPolicy); err != nil {
				return err
			}
		}
		communityConfig := community.ConfigSnapshot{}
		for _, m := range c.Members {
			communityConfig.Members = append(communityConfig.Members, community.Member{PubKey: m.PubKey, Name: m.Name, Note: m.Note, Role: m.Role})
		}
		for _, b := range c.Bans {
			pk, _ := b["pubkey"].(string)
			reason, _ := b["reason"].(string)
			communityConfig.Bans = append(communityConfig.Bans, community.ConfigBan{PubKey: pk, Reason: reason})
		}
		for _, b := range c.Addresses {
			ip, _ := b["ip"].(string)
			reason, _ := b["reason"].(string)
			communityConfig.IPBlocks = append(communityConfig.IPBlocks, community.ConfigIPBlock{IP: ip, Reason: reason})
		}
		for _, b := range c.BannedEvents {
			id, _ := b["id"].(string)
			reason, _ := b["reason"].(string)
			communityConfig.EventBans = append(communityConfig.EventBans, community.ConfigEventBan{ID: id, Reason: reason})
		}
		for _, k := range c.Kinds.Allow {
			communityConfig.KindRules = append(communityConfig.KindRules, community.ConfigKindRule{Kind: k, Rule: "allow"})
		}
		for _, k := range c.Kinds.Block {
			communityConfig.KindRules = append(communityConfig.KindRules, community.ConfigKindRule{Kind: k, Rule: "block"})
		}
		for _, r := range c.Retention {
			communityConfig.Retention = append(communityConfig.Retention, community.ConfigRetention{Kind: r.Kind, Days: r.Days})
		}
		if err := community.ApplyConfigTx(ctx, tx, communityConfig, has(c, "members"), has(c, "bans"), has(c, "addresses"), has(c, "banned_events"), has(c, "kinds"), has(c, "retention")); err != nil {
			return err
		}
		if s.Community != nil && ch.FinalPolicy.Owner != "" && ch.FinalPolicy.Owner != cur.Owner {
			if err := s.Community.ApplyOwnerTx(ctx, tx, cur.Owner, ch.FinalPolicy.Owner); err != nil {
				return err
			}
		}
		if has(c, "connections") {
			if _, err := tx.ExecContext(ctx, `DELETE FROM config_connections`); err != nil {
				return err
			}
			for i, conn := range c.Connections {
				b, _ := json.Marshal(conn)
				if _, err := tx.ExecContext(ctx, `INSERT INTO config_connections(position,value) VALUES(?,?)`, i, string(b)); err != nil {
					return err
				}
			}
		}
		if has(c, "jobs") {
			if _, err := tx.ExecContext(ctx, `DELETE FROM config_jobs`); err != nil {
				return err
			}
			var replicationJobs []replication.ConfigJob
			for _, job := range c.Jobs {
				id, _ := job["id"].(string)
				if id == "" {
					return errors.New("config jobs: missing id")
				}
				copy := make(map[string]any, len(job))
				for k, v := range job {
					if k != "id" {
						copy[k] = v
					}
				}
				recipe, err := json.Marshal(copy)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO config_jobs(id,recipe) VALUES(?,?)`, id, string(recipe)); err != nil {
					return err
				}
				copy["id"] = id
				payload, err := json.Marshal(copy)
				if err != nil {
					return err
				}
				replicationJobs = append(replicationJobs, replication.ConfigJob{ID: id, Payload: payload})
			}
			if err := replication.ReplaceJobsTx(ctx, tx, replicationJobs); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Changes{}, fmt.Errorf("config apply: %w", err)
	}
	if ch.FinalPolicy.Owner != cur.Owner && s.Community != nil {
		if err := s.Community.SetOwner(ch.FinalPolicy.Owner); err != nil {
			return Changes{}, err
		}
	}
	if (len(c.Policy) > 0 || ch.FinalPolicy.Owner != cur.Owner) && s.OnApplied != nil {
		s.OnApplied(ch.FinalPolicy)
	}
	return ch, nil
}

func Presets() []string { return templates.Names() }

func (s *ConfigStore) Execute(ctx context.Context, method string, params []json.RawMessage) (any, error) {
	switch method {
	case "exportconfig":
		return s.Export(ctx)
	case "importconfig":
		if len(params) == 0 {
			return nil, errors.New("invalid: missing configuration")
		}
		var raw json.RawMessage = params[0]
		c, err := Parse(raw)
		if err != nil {
			return nil, err
		}
		dry := false
		migrationOwner := ""
		if len(params) > 1 {
			var o struct {
				DryRun         bool   `json:"dryRun"`
				MigrationOwner string `json:"migrationOwner"`
			}
			_ = json.Unmarshal(params[1], &o)
			dry = o.DryRun
			migrationOwner = o.MigrationOwner
		}
		return s.ApplyWithOptions(ctx, c, ApplyOptions{DryRun: dry, MigrationOwner: migrationOwner})
	case "applypreset":
		var name string
		if len(params) == 0 || json.Unmarshal(params[0], &name) != nil {
			return nil, errors.New("invalid: preset name required")
		}
		return s.ApplyPreset(ctx, name, false)
	case "listpresets":
		out := []map[string]any{}
		for _, name := range templates.Names() {
			if t, ok := templates.Find(name); ok {
				out = append(out, map[string]any{"name": t.Name, "title": t.Title, "about": t.About, "source": t.Source, "every": t.EveryHours, "connections": t.Connections})
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported: unknown config method %q", method)
	}
}

func (s *ConfigStore) ApplyPreset(ctx context.Context, name string, dryRun bool) (Changes, error) {
	t, ok := templates.Find(name)
	if !ok {
		return Changes{}, fmt.Errorf("invalid: no preset named %s", name)
	}
	p := policy.Defaults(s.Policy().Owner)
	_ = p
	raw, _ := json.Marshal(t.Policy)
	var pm map[string]json.RawMessage
	_ = json.Unmarshal(raw, &pm)
	c := Config{Format: Format, Policy: pm, Sections: []string{"policy"}}
	if len(t.AllowKinds) > 0 || len(t.BlockKinds) > 0 {
		c.Sections = append(c.Sections, "kinds")
		c.Kinds.Allow = t.AllowKinds
		c.Kinds.Block = t.BlockKinds
	}
	if t.RetentionDays > 0 {
		c.Sections = append(c.Sections, "retention")
		c.Retention = []Rule{{Days: t.RetentionDays}}
	}
	if len(t.Connections) > 0 {
		c.Sections = append(c.Sections, "connections")
		for _, name := range t.Connections {
			c.Connections = append(c.Connections, map[string]any{"template": name})
		}
	}
	return s.Apply(ctx, c, dryRun)
}
