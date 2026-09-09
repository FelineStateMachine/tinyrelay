package community

// Agent identities. An agent is a member with role "agent" that publishes
// under a grant signed by the owner or a moderator. The grant is a normal
// addressable event (kind 30392) mirrored into agent_grants so the write gate
// can answer "may this key publish this event" with one indexed lookup. The
// agent stays the author of everything it publishes; the relay only decides
// whether to keep it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
)

const (
	AgentRateDefault  = 60
	AgentRateMax      = 600
	AgentGrantMaxDays = 365
	AgentSiteTTLMax   = 365
	agentNameMax      = 64
	agentRoomMax      = 128
	agentListMax      = 256
)

const agentSchema = `CREATE TABLE IF NOT EXISTS agent_grants(agent TEXT PRIMARY KEY,owner TEXT NOT NULL,event_id TEXT NOT NULL,name TEXT NOT NULL DEFAULT '',expires_at INTEGER NOT NULL,paused INTEGER NOT NULL DEFAULT 0,revoked_at INTEGER NOT NULL DEFAULT 0,scope TEXT NOT NULL DEFAULT '{}');`

// AgentRepo is one repository an agent may work in. Level is "propose",
// "read" or "maintain". Propose and read publish the same kinds, but each
// event from a propose grant is a proposal that waits for a maintainer's
// decision; maintain adds status changes and pushes.
type AgentRepo struct {
	Owner      string `json:"owner"`
	Identifier string `json:"identifier"`
	Level      string `json:"level"`
}

// Repository grant levels.
const (
	AgentRepoPropose  = "propose"
	AgentRepoRead     = "read"
	AgentRepoMaintain = "maintain"
)

// Long task grants. AgentJobsRequest lets the agent publish job requests,
// AgentJobsServe lets it answer requests the relay holds with results and
// feedback, and AgentJobsBoth does both.
const (
	AgentJobsRequest = "request"
	AgentJobsServe   = "serve"
	AgentJobsBoth    = "both"
)

// AgentSite is one static site an agent may publish. Label is a site label
// under the agent's own key or "*" for any of them. TTLDays, when set, bounds
// how long a manifest and the files behind it live; Encrypted requires the
// agent's uploads to be encrypted.
type AgentSite struct {
	Label     string `json:"label"`
	TTLDays   int    `json:"ttl,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
}

// AgentScope is the part of a grant the gate checks on every event.
type AgentScope struct {
	Kinds []int       `json:"kinds"`
	Rooms []string    `json:"rooms"`
	Repos []AgentRepo `json:"repos"`
	Wiki  string      `json:"wiki,omitempty"`
	Jobs  string      `json:"jobs,omitempty"`
	Sites []AgentSite `json:"sites,omitempty"`
	Rate  int         `json:"rate"`
}

// AgentGrant mirrors one grant event plus the owner's pause and revoke marks.
type AgentGrant struct {
	Agent     string     `json:"pubkey"`
	Owner     string     `json:"owner"`
	EventID   string     `json:"event_id"`
	Name      string     `json:"name"`
	ExpiresAt int64      `json:"expires"`
	Paused    bool       `json:"paused"`
	RevokedAt int64      `json:"revoked"`
	Scope     AgentScope `json:"scope"`
}

// AgentSummary is the management view of a grant.
type AgentSummary struct {
	AgentGrant
	LastEvent int64 `json:"lastEvent"`
}

// Active reports whether the grant currently lets the agent publish.
func (g AgentGrant) Active(now int64) bool {
	return !g.Paused && g.RevokedAt == 0 && g.ExpiresAt > now
}

// Maintains reports whether the grant currently makes the agent a maintainer
// of one repository: it is active and holds maintain on that exact repository.
func (g AgentGrant) Maintains(owner, identifier string, now int64) bool {
	return g.Active(now) && g.RepoLevel(owner, identifier) == AgentRepoMaintain
}

// Proposes reports whether the grant holds propose on one repository, so
// the agent's issues, pull requests, patches and comments there are
// proposals. A paused, revoked or expired grant keeps counting, so an
// agent's pending events do not surface when its grant ends.
func (g AgentGrant) Proposes(owner, identifier string) bool {
	return g.RepoLevel(owner, identifier) == AgentRepoPropose
}

// AllowsKind reports whether the agent may publish this kind. Profiles, relay
// lists and auth are always allowed so an agent can identify itself.
func (g AgentGrant) AllowsKind(kind int) bool {
	if kind == event.KIND_PROFILE || kind == 10002 || kind == event.KIND_AUTH {
		return true
	}
	for _, allowed := range g.Scope.Kinds {
		if allowed == kind {
			return true
		}
	}
	if event.IsJobRequest(kind) && g.RequestsJobs() {
		return true
	}
	if (event.IsJobResult(kind) || kind == event.KIND_JOB_FEEDBACK) && g.ServesJobs() {
		return true
	}
	if siteManifestKind(kind) && len(g.Scope.Sites) > 0 {
		return true
	}
	// Wiki access carries its kinds: proposing means publishing a version and
	// asking for a merge; editing adds redirects.
	switch g.Scope.Wiki {
	case "propose":
		return kind == event.KIND_WIKI_ARTICLE || kind == event.KIND_WIKI_MERGE
	case "edit":
		return kind == event.KIND_WIKI_ARTICLE || kind == event.KIND_WIKI_MERGE || kind == event.KIND_WIKI_REDIRECT
	}
	return false
}

// siteManifestKind reports whether a kind is a site manifest a grant's sites
// tag admits: the key's own site and named sites under it. Snapshots are
// left out; they are copies of a site rather than a site the agent publishes.
func siteManifestKind(kind int) bool {
	return kind == sites.KindSite || kind == sites.KindNamedSite
}

// SiteEntries lists the grant's site entries that cover one label.
func (g AgentGrant) SiteEntries(label string) []AgentSite {
	var out []AgentSite
	for _, site := range g.Scope.Sites {
		if site.Label == "*" || (label != "" && site.Label == label) {
			out = append(out, site)
		}
	}
	return out
}

// UploadTTLDays is the number of days the agent's uploads live: the longest
// ttl among its site entries, or 0 when there is none or an entry sets none.
func (g AgentGrant) UploadTTLDays() int { return longestTTL(g.Scope.Sites) }

// UploadsEncrypted reports whether the agent's uploads must be encrypted.
// One entry with the flag applies it to every upload the agent makes, since
// an upload does not say which site it is for.
func (g AgentGrant) UploadsEncrypted() bool {
	for _, site := range g.Scope.Sites {
		if site.Encrypted {
			return true
		}
	}
	return false
}

func longestTTL(entries []AgentSite) int {
	longest := 0
	for _, site := range entries {
		if site.TTLDays == 0 {
			return 0
		}
		if site.TTLDays > longest {
			longest = site.TTLDays
		}
	}
	return longest
}

// RequestsJobs reports whether the jobs tag lets the agent publish job
// requests.
func (g AgentGrant) RequestsJobs() bool {
	return g.Scope.Jobs == AgentJobsRequest || g.Scope.Jobs == AgentJobsBoth
}

// ServesJobs reports whether the jobs tag lets the agent answer job
// requests with results and feedback.
func (g AgentGrant) ServesJobs() bool {
	return g.Scope.Jobs == AgentJobsServe || g.Scope.Jobs == AgentJobsBoth
}

// AllowsRoom reports whether the agent may publish into a room.
func (g AgentGrant) AllowsRoom(room string) bool {
	for _, allowed := range g.Scope.Rooms {
		if allowed == room {
			return true
		}
	}
	return false
}

// RepoLevel returns the agent's level in a repository, or "" when the
// repository is outside the grant.
func (g AgentGrant) RepoLevel(owner, identifier string) string {
	for _, repo := range g.Scope.Repos {
		if repo.Owner == owner && repo.Identifier == identifier {
			return repo.Level
		}
	}
	return ""
}

// Check applies the grant to one event and returns a NIP-01 rejection when
// the grant does not cover it.
func (g AgentGrant) Check(e event.Event, now int64) error {
	switch {
	case g.RevokedAt > 0:
		return errors.New("restricted: agent grant is revoked")
	case g.Paused:
		return errors.New("restricted: agent grant is paused")
	case g.ExpiresAt <= now:
		return errors.New("restricted: agent grant has expired")
	}
	if !g.AllowsKind(e.Kind) {
		return fmt.Errorf("restricted: agent grant does not allow kind %d", e.Kind)
	}
	if room := event.Tag(e, "h"); room != "" && !g.AllowsRoom(room) {
		return errors.New("restricted: agent grant does not allow room " + room)
	}
	if siteManifestKind(e.Kind) {
		return g.checkSite(e, now)
	}
	if !gitKind(e.Kind) {
		return nil
	}
	coordinates := repositoryCoordinates(e)
	if len(coordinates) == 0 {
		if e.Kind == 1111 {
			return nil
		}
		return errors.New("restricted: agent grant does not allow repository events without a repository address")
	}
	for _, coordinate := range coordinates {
		level := g.RepoLevel(coordinate[0], coordinate[1])
		if level == "" {
			return fmt.Errorf("restricted: agent grant does not allow repository %s:%s", coordinate[0], coordinate[1])
		}
		if gitStatusKind(e.Kind) && level != AgentRepoMaintain {
			return fmt.Errorf("restricted: agent grant does not allow status changes in repository %s:%s", coordinate[0], coordinate[1])
		}
	}
	return nil
}

// checkSite applies the sites entries to a manifest: the label must be
// covered, and when the covering entries carry a ttl the manifest must
// expire within it so NIP-40 retires the site on time.
func (g AgentGrant) checkSite(e event.Event, now int64) error {
	label := sites.SiteLabel(e)
	entries := g.SiteEntries(label)
	if len(entries) == 0 {
		return fmt.Errorf("restricted: agent grant does not allow site %s", label)
	}
	ttl := longestTTL(entries)
	if ttl == 0 {
		return nil
	}
	latest := now + int64(ttl)*86400
	expires := event.Expiration(e)
	if expires == 0 {
		return fmt.Errorf("invalid: agent grant allows site %s for %d days: add an expiration tag no later than %d", label, ttl, latest)
	}
	if expires > latest {
		return fmt.Errorf("invalid: agent grant allows site %s for %d days: set the expiration tag no later than %d", label, ttl, latest)
	}
	return nil
}

func gitKind(kind int) bool {
	return kind == event.KIND_GIT_PATCH || kind == event.KIND_GIT_PR || kind == event.KIND_GIT_PR_UPDATE || kind == event.KIND_GIT_ISSUE || kind == 1111 || gitStatusKind(kind)
}

func gitStatusKind(kind int) bool { return kind >= 1630 && kind <= 1633 }

// repositoryCoordinates lists the 30617 owner and identifier pairs an event
// points at through its a or A tags.
func repositoryCoordinates(e event.Event) [][2]string {
	var out [][2]string
	for _, tag := range e.Tags {
		if len(tag) < 2 || (tag[0] != "a" && tag[0] != "A") {
			continue
		}
		parts := strings.SplitN(tag[1], ":", 3)
		if len(parts) != 3 || parts[0] != strconv.Itoa(event.KIND_REPO) || !validPubKey(parts[1]) || parts[2] == "" {
			continue
		}
		out = append(out, [2]string{parts[1], parts[2]})
	}
	return out
}

// ParseAgentGrant validates a kind 30392 event and returns the grant it
// describes. Signer authority is checked by the caller.
func ParseAgentGrant(e event.Event, now int64) (AgentGrant, error) {
	if e.Kind != event.KIND_AGENT_GRANT {
		return AgentGrant{}, fmt.Errorf("invalid: kind %d is not an agent grant", e.Kind)
	}
	agent := event.Tag(e, "d")
	if !validPubKey(agent) {
		return AgentGrant{}, errors.New("invalid: agent grant needs the agent pubkey as its d tag")
	}
	if event.Tag(e, "p") != agent {
		return AgentGrant{}, errors.New("invalid: agent grant p tag must match its d tag")
	}
	if agent == e.PubKey {
		return AgentGrant{}, errors.New("invalid: agent grant may not name its signer")
	}
	expires := event.Expiration(e)
	if expires == 0 {
		return AgentGrant{}, errors.New("invalid: agent grant needs an expiration tag")
	}
	if expires > now+AgentGrantMaxDays*86400 {
		return AgentGrant{}, fmt.Errorf("invalid: agent grant expiration is more than %d days out", AgentGrantMaxDays)
	}
	if e.Content != "" && !json.Valid([]byte(e.Content)) {
		return AgentGrant{}, errors.New("invalid: agent grant content must be empty or JSON")
	}
	grant := AgentGrant{Agent: agent, Owner: e.PubKey, EventID: e.ID, ExpiresAt: expires, Scope: AgentScope{Kinds: []int{}, Rooms: []string{}, Repos: []AgentRepo{}, Rate: AgentRateDefault}}
	grant.Name = strings.TrimSpace(event.Tag(e, "name"))
	if len(grant.Name) > agentNameMax {
		grant.Name = grant.Name[:agentNameMax]
	}
	seenRoom := map[string]bool{}
	for _, room := range event.TagValues(e, "room") {
		room = strings.TrimSpace(room)
		if room == "" || len(room) > agentRoomMax {
			return AgentGrant{}, errors.New("invalid: agent grant room tag")
		}
		if !seenRoom[room] {
			seenRoom[room] = true
			grant.Scope.Rooms = append(grant.Scope.Rooms, room)
		}
	}
	for _, value := range event.TagValues(e, "repo") {
		repo, ok := parseAgentRepo(value)
		if !ok {
			return AgentGrant{}, errors.New("invalid: agent grant repo tag must be owner:identifier followed by :propose, :read or :maintain")
		}
		if grant.RepoLevel(repo.Owner, repo.Identifier) == "" {
			grant.Scope.Repos = append(grant.Scope.Repos, repo)
		}
	}
	seenKind := map[int]bool{}
	for _, value := range event.TagValues(e, "k") {
		kind, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || kind < 0 || kind > 65535 {
			return AgentGrant{}, errors.New("invalid: agent grant k tag must be an event kind")
		}
		if !seenKind[kind] {
			seenKind[kind] = true
			grant.Scope.Kinds = append(grant.Scope.Kinds, kind)
		}
	}
	if len(grant.Scope.Rooms) > agentListMax || len(grant.Scope.Repos) > agentListMax || len(grant.Scope.Kinds) > agentListMax {
		return AgentGrant{}, fmt.Errorf("invalid: agent grant lists more than %d entries", agentListMax)
	}
	switch wiki := event.Tag(e, "wiki"); wiki {
	case "", "propose", "edit":
		grant.Scope.Wiki = wiki
	default:
		return AgentGrant{}, errors.New("invalid: agent grant wiki tag must be propose or edit")
	}
	switch jobs := event.Tag(e, "jobs"); jobs {
	case "", AgentJobsRequest, AgentJobsServe, AgentJobsBoth:
		grant.Scope.Jobs = jobs
	default:
		return AgentGrant{}, errors.New("invalid: agent grant jobs tag must be request, serve or both")
	}
	for _, tag := range e.Tags {
		if len(tag) == 0 || tag[0] != "sites" {
			continue
		}
		site, err := parseAgentSite(tag[1:], agent)
		if err != nil {
			return AgentGrant{}, err
		}
		if !siteListed(grant.Scope.Sites, site.Label) {
			grant.Scope.Sites = append(grant.Scope.Sites, site)
		}
	}
	if len(grant.Scope.Sites) > agentListMax {
		return AgentGrant{}, fmt.Errorf("invalid: agent grant lists more than %d entries", agentListMax)
	}
	if value := event.Tag(e, "rate"); value != "" {
		rate, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || rate < 1 || rate > AgentRateMax {
			return AgentGrant{}, fmt.Errorf("invalid: agent grant rate must be between 1 and %d events per minute", AgentRateMax)
		}
		grant.Scope.Rate = rate
	}
	return grant, nil
}

func parseAgentRepo(value string) (AgentRepo, bool) {
	value = strings.TrimSpace(value)
	last := strings.LastIndex(value, ":")
	if last < 0 {
		return AgentRepo{}, false
	}
	level := value[last+1:]
	if level != AgentRepoPropose && level != AgentRepoRead && level != AgentRepoMaintain {
		return AgentRepo{}, false
	}
	parts := strings.SplitN(value[:last], ":", 2)
	if len(parts) != 2 || !validPubKey(parts[0]) || parts[1] == "" || len(parts[1]) > 256 {
		return AgentRepo{}, false
	}
	return AgentRepo{Owner: parts[0], Identifier: parts[1], Level: level}, true
}

// parseAgentSite reads one sites tag: a label, then any of ttl=<days> and
// encrypted. The label is "*" or a site label under the agent's own key.
func parseAgentSite(values []string, agent string) (AgentSite, error) {
	if len(values) == 0 {
		return AgentSite{}, errors.New("invalid: agent grant sites tag needs a site label or *")
	}
	site := AgentSite{Label: strings.TrimSpace(values[0])}
	if site.Label != "*" {
		parsed, ok := sites.ParseSite(site.Label)
		if !ok || parsed.Kind == sites.KindSiteSnapshot {
			return AgentSite{}, errors.New("invalid: agent grant sites tag needs a site label or *")
		}
		if parsed.PubKey != agent {
			return AgentSite{}, errors.New("invalid: agent grant sites label must be a site under the agent's own key")
		}
	}
	for _, flag := range values[1:] {
		flag = strings.TrimSpace(flag)
		switch {
		case flag == "encrypted":
			site.Encrypted = true
		case strings.HasPrefix(flag, "ttl="):
			days, err := strconv.Atoi(strings.TrimPrefix(flag, "ttl="))
			if err != nil || days < 1 || days > AgentSiteTTLMax {
				return AgentSite{}, fmt.Errorf("invalid: agent grant sites ttl must be between 1 and %d days", AgentSiteTTLMax)
			}
			site.TTLDays = days
		default:
			return AgentSite{}, errors.New("invalid: agent grant sites tag allows only ttl=<days> and encrypted after the label")
		}
	}
	return site, nil
}

func siteListed(entries []AgentSite, label string) bool {
	for _, site := range entries {
		if site.Label == label {
			return true
		}
	}
	return false
}

// CanGrantAgents reports whether a role may sign agent grants.
func CanGrantAgents(role string) bool { return role == "owner" || role == "moderator" }

// AgentGrant returns the grant recorded for a pubkey, if any.
func (s *Service) AgentGrant(ctx context.Context, pubkey string) (AgentGrant, bool, error) {
	var grant AgentGrant
	var paused int
	var scope string
	err := s.store.DB().QueryRowContext(ctx, `SELECT agent,owner,event_id,name,expires_at,paused,revoked_at,scope FROM agent_grants WHERE agent=?`, pubkey).Scan(&grant.Agent, &grant.Owner, &grant.EventID, &grant.Name, &grant.ExpiresAt, &paused, &grant.RevokedAt, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentGrant{}, false, nil
	}
	if err != nil {
		return AgentGrant{}, false, fmt.Errorf("agent grant: %w", err)
	}
	grant.Paused = paused != 0
	if err := json.Unmarshal([]byte(scope), &grant.Scope); err != nil {
		return AgentGrant{}, false, fmt.Errorf("agent grant scope: %w", err)
	}
	return grant, true, nil
}

// ApplyAgentEventTx mirrors a grant or a deletion into agent_grants and the
// member roster inside the transaction that stores the event, so the role and
// the event land or roll back together.
func (s *Service) ApplyAgentEventTx(ctx context.Context, tx *sql.Tx, e event.Event, now int64) error {
	switch e.Kind {
	case event.KIND_AGENT_GRANT:
		grant, err := ParseAgentGrant(e, now)
		if err != nil {
			return err
		}
		return s.applyAgentGrantTx(ctx, tx, grant, now)
	case event.KIND_DELETION:
		return s.revokeDeletedGrantsTx(ctx, tx, e, now)
	}
	return nil
}

func (s *Service) applyAgentGrantTx(ctx context.Context, tx *sql.Tx, grant AgentGrant, now int64) error {
	scope, err := json.Marshal(grant.Scope)
	if err != nil {
		return err
	}
	revokedAt := int64(0)
	if grant.ExpiresAt <= now {
		revokedAt = now
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_grants(agent,owner,event_id,name,expires_at,paused,revoked_at,scope) VALUES(?,?,?,?,?,0,?,?) ON CONFLICT(agent) DO UPDATE SET owner=excluded.owner,event_id=excluded.event_id,name=excluded.name,expires_at=excluded.expires_at,paused=0,revoked_at=excluded.revoked_at,scope=excluded.scope`, grant.Agent, grant.Owner, grant.EventID, grant.Name, grant.ExpiresAt, revokedAt, string(scope)); err != nil {
		return fmt.Errorf("record agent grant: %w", err)
	}
	if revokedAt > 0 {
		if err := s.dropAgentRoleTx(ctx, tx, grant.Agent); err != nil {
			return err
		}
		return s.recordTx(ctx, tx, grant.Owner, "revokeagent", grant.Agent, "expired grant")
	}
	// A key that already holds a human role keeps it; the grant only adds the
	// agent role to keys that have no other standing here.
	if _, err := tx.ExecContext(ctx, `INSERT INTO community_members(pubkey,role,invited_by,via,created_at) VALUES(?,?,?,?,?) ON CONFLICT(pubkey) DO NOTHING`, grant.Agent, "agent", grant.Owner, "grant", now); err != nil {
		return fmt.Errorf("add agent member: %w", err)
	}
	return s.recordTx(ctx, tx, grant.Owner, "grantagent", grant.Agent, grant.Name)
}

// revokeDeletedGrantsTx revokes grants a kind 5 names by event id or by
// 30392 address. Only the grant's own signer can revoke it this way.
func (s *Service) revokeDeletedGrantsTx(ctx context.Context, tx *sql.Tx, e event.Event, now int64) error {
	prefix := strconv.Itoa(event.KIND_AGENT_GRANT) + ":" + e.PubKey + ":"
	for _, tag := range e.Tags {
		if len(tag) < 2 {
			continue
		}
		var agent string
		switch tag[0] {
		case "a":
			if !strings.HasPrefix(tag[1], prefix) {
				continue
			}
			agent = strings.TrimPrefix(tag[1], prefix)
		case "e":
			if err := tx.QueryRowContext(ctx, `SELECT agent FROM agent_grants WHERE event_id=? AND owner=?`, tag[1], e.PubKey).Scan(&agent); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return err
			}
		default:
			continue
		}
		if !validPubKey(agent) {
			continue
		}
		if err := s.revokeAgentTx(ctx, tx, e.PubKey, agent, now, "deleted grant"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) revokeAgentTx(ctx context.Context, tx *sql.Tx, actor, agent string, now int64, detail string) error {
	result, err := tx.ExecContext(ctx, `UPDATE agent_grants SET revoked_at=? WHERE agent=? AND owner=? AND revoked_at=0`, now, agent, actor)
	if err != nil {
		return fmt.Errorf("revoke agent grant: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return nil
	}
	if err := s.dropAgentRoleTx(ctx, tx, agent); err != nil {
		return err
	}
	return s.recordTx(ctx, tx, actor, "revokeagent", agent, detail)
}

func (s *Service) dropAgentRoleTx(ctx context.Context, tx *sql.Tx, agent string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM community_members WHERE pubkey=? AND role='agent'`, agent); err != nil {
		return fmt.Errorf("remove agent role: %w", err)
	}
	return nil
}

// Agents lists every grant with the time of the agent's newest stored event.
func (s *Service) Agents(ctx context.Context) ([]AgentSummary, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT g.agent,g.owner,g.event_id,g.name,g.expires_at,g.paused,g.revoked_at,g.scope,(SELECT coalesce(max(created_at),0) FROM events WHERE pubkey=g.agent) FROM agent_grants g ORDER BY g.name,g.agent`)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	out := []AgentSummary{}
	for rows.Next() {
		var item AgentSummary
		var paused int
		var scope string
		if err := rows.Scan(&item.Agent, &item.Owner, &item.EventID, &item.Name, &item.ExpiresAt, &paused, &item.RevokedAt, &scope, &item.LastEvent); err != nil {
			return nil, err
		}
		item.Paused = paused != 0
		if err := json.Unmarshal([]byte(scope), &item.Scope); err != nil {
			return nil, fmt.Errorf("agent grant scope: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// RepositoryAgents lists the active grants that hold maintain on one
// repository, in name order. Paused, revoked and expired grants are left out.
func (s *Service) RepositoryAgents(ctx context.Context, owner, identifier string, now int64) ([]AgentGrant, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT agent,owner,event_id,name,expires_at,scope FROM agent_grants WHERE paused=0 AND revoked_at=0 AND expires_at>? ORDER BY name,agent`, now)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, fmt.Errorf("list repository agents: %w", err)
	}
	defer rows.Close()
	var out []AgentGrant
	for rows.Next() {
		var grant AgentGrant
		var scope string
		if err := rows.Scan(&grant.Agent, &grant.Owner, &grant.EventID, &grant.Name, &grant.ExpiresAt, &scope); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scope), &grant.Scope); err != nil {
			return nil, fmt.Errorf("agent grant scope: %w", err)
		}
		if grant.Maintains(owner, identifier, now) {
			out = append(out, grant)
		}
	}
	return out, rows.Err()
}

// ActiveAgentCount counts grants that currently allow publishing.
func (s *Service) ActiveAgentCount(ctx context.Context, now int64) (int, error) {
	var count int
	err := s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM agent_grants WHERE paused=0 AND revoked_at=0 AND expires_at>?`, now).Scan(&count)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return 0, nil
	}
	return count, err
}

// SetAgentPaused pauses or resumes one agent without touching its grant event.
func (s *Service) SetAgentPaused(ctx context.Context, actor, agent string, paused bool) (AgentGrant, error) {
	if !validPubKey(agent) {
		return AgentGrant{}, errors.New("invalid: bad pubkey")
	}
	grant, ok, err := s.AgentGrant(ctx, agent)
	if err != nil {
		return AgentGrant{}, err
	}
	if !ok {
		return AgentGrant{}, errors.New("invalid: no agent grant for this pubkey")
	}
	if grant.RevokedAt > 0 {
		return AgentGrant{}, errors.New("invalid: agent grant is revoked")
	}
	action, value := "resumeagent", 0
	if paused {
		action, value = "pauseagent", 1
	}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_grants SET paused=? WHERE agent=?`, value, agent); err != nil {
			return err
		}
		return s.recordTx(ctx, tx, actor, action, agent, "")
	})
	if err != nil {
		return AgentGrant{}, err
	}
	grant.Paused = paused
	return grant, nil
}

// RevokeAgent marks a grant revoked and removes the agent role. The grant
// event stays stored for audit.
func (s *Service) RevokeAgent(ctx context.Context, actor, agent string, now int64) (AgentGrant, error) {
	if !validPubKey(agent) {
		return AgentGrant{}, errors.New("invalid: bad pubkey")
	}
	grant, ok, err := s.AgentGrant(ctx, agent)
	if err != nil {
		return AgentGrant{}, err
	}
	if !ok {
		return AgentGrant{}, errors.New("invalid: no agent grant for this pubkey")
	}
	if grant.RevokedAt > 0 {
		return grant, nil
	}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_grants SET revoked_at=? WHERE agent=?`, now, agent); err != nil {
			return err
		}
		if err := s.dropAgentRoleTx(ctx, tx, agent); err != nil {
			return err
		}
		return s.recordTx(ctx, tx, actor, "revokeagent", agent, "management")
	})
	if err != nil {
		return AgentGrant{}, err
	}
	grant.RevokedAt = now
	return grant, nil
}

// SetAllAgentsPaused is the kill switch. It returns how many grants changed.
func (s *Service) SetAllAgentsPaused(ctx context.Context, actor string, paused bool) (int, error) {
	action, value := "resumeallagents", 0
	if paused {
		action, value = "pauseallagents", 1
	}
	changed := 0
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE agent_grants SET paused=? WHERE revoked_at=0 AND paused<>?`, value, value)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		changed = int(n)
		return s.recordTx(ctx, tx, actor, action, "", strconv.Itoa(changed))
	})
	return changed, err
}
