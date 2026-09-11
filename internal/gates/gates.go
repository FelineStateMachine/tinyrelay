package gates

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/dlclark/regexp2"
)

// Config supplies the policy and optional durable services used by Gate.
// Store and Community enable checks that depend on persisted relay state.
type Config struct {
	Store     *storage.Store
	Community *community.Service
	Policy    func() policy.Policy
	Slug      string
}

// Gate evaluates event admission and visibility for one tenant configuration.
type Gate struct {
	cfg    Config
	agents agentLimiter
}

// agentLimiter is the per-agent sliding one-minute counter. It lives in
// memory: a restart forgives the last minute, which is the cheap and honest
// trade for a limit that never touches the database on the hot path.
type agentLimiter struct {
	mu     sync.Mutex
	recent map[string][]int64
}

func (l *agentLimiter) allow(agent string, limit int, now int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.recent == nil {
		l.recent = map[string][]int64{}
	}
	kept := l.recent[agent][:0]
	for _, at := range l.recent[agent] {
		if at > now-60 {
			kept = append(kept, at)
		}
	}
	if len(kept) >= limit {
		l.recent[agent] = kept
		return false
	}
	l.recent[agent] = append(kept, now)
	return true
}

// agentAdmission applies an agent grant to an event from a key whose only
// standing here is that grant. Keys with a human role keep that role's
// permissions. Events from agents are never stamped or rewritten; they are
// kept as signed or refused with a restricted reason.
func (g *Gate) agentAdmission(ctx context.Context, e event.Event, now int64) error {
	if g.cfg.Community == nil {
		return nil
	}
	grant, ok, err := g.cfg.Community.AgentGrant(ctx, e.PubKey)
	if err != nil {
		return fmt.Errorf("check agent grant: %w", err)
	}
	if !ok {
		if community.IsAgentGrantRequest(e) {
			return errors.New("restricted: grant requests require an active agent grant")
		}
		return nil
	}
	role, err := g.cfg.Community.Role(ctx, e.PubKey)
	if err != nil {
		return fmt.Errorf("check agent role: %w", err)
	}
	if role != "" && role != "agent" {
		if community.IsAgentGrantRequest(e) {
			return errors.New("restricted: only the granted agent may request a grant change")
		}
		return nil
	}
	if community.IsAgentGrantRequest(e) {
		if _, reviewErr := g.cfg.Community.ReviewAgentGrantRequest(ctx, e, now); reviewErr != nil {
			return reviewErr
		}
	}
	if !community.IsAgentGrantRequest(e) {
		if err := grant.Check(e, now); err != nil {
			return err
		}
	}
	if err := g.jobReply(ctx, e, now, true); err != nil {
		return err
	}
	if !g.agents.allow(e.PubKey, grant.Scope.Rate, now) {
		return fmt.Errorf("restricted: agent grant does not allow more than %d events per minute", grant.Scope.Rate)
	}
	return nil
}

// agentGrantShape checks a grant event's signer and tags before it is stored.
func (g *Gate) agentGrantShape(ctx context.Context, e event.Event, now int64) error {
	if e.Kind != event.KIND_AGENT_GRANT {
		return nil
	}
	if g.cfg.Community == nil {
		return errors.New("restricted: agent grants need the membership service")
	}
	role, err := g.cfg.Community.Role(ctx, e.PubKey)
	if err != nil {
		return fmt.Errorf("check grant signer: %w", err)
	}
	if !community.CanGrantAgents(role) {
		return errors.New("restricted: only the owner or a moderator can grant an agent")
	}
	if event.Tag(e, "grant-request") != "" || event.Tag(e, "grant-base") != "" {
		if err := g.cfg.Community.ValidateAgentGrantReplacement(ctx, e, now); err != nil {
			return err
		}
	}
	_, err = community.ParseAgentGrant(e, now)
	return err
}

func New(cfg Config) (*Gate, error) {
	if cfg.Policy == nil {
		return nil, errors.New("gates: policy function is required")
	}
	return &Gate{cfg: cfg}, nil
}

// Write validates a client-published event against protocol shape, tenant
// policy, membership, bans, room access, retention and proof-of-work rules.
func (g *Gate) Write(ctx context.Context, e event.Event, s relay.Session, now int64) error {
	p := g.cfg.Policy()
	if e.Kind == event.KIND_AUTH {
		return errors.New("blocked: kind 22242 is only accepted inside AUTH")
	}
	if e.Kind == event.KIND_MARMOT_GROUP || e.Kind == event.KIND_MARMOT_KEY_PACKAGE {
		if !p.Features.Marmot {
			return errors.New("restricted: Marmot transport is switched off on this relay")
		}
		if err := MarmotShape(e); err != nil {
			return err
		}
		if e.Kind == event.KIND_MARMOT_GROUP {
			if contains(s.PubKeys, e.PubKey) {
				return errors.New("invalid: kind 445 must use a fresh ephemeral author")
			}
			if g.cfg.Store != nil {
				result, err := g.cfg.Store.Query(ctx, event.Filter{Kinds: []int{event.KIND_MARMOT_GROUP}, Authors: []string{e.PubKey}}, storage.QueryOptions{Now: now, Access: storage.Access{All: true}, Limit: 1})
				if err != nil {
					return fmt.Errorf("check Marmot ephemeral author: %w", err)
				}
				if len(result.Events) > 0 {
					return errors.New("invalid: kind 445 ephemeral author was already used")
				}
			}
		}
	}
	if err := event.Validate(e); err != nil {
		return err
	}
	if err := nip43Shape(e, now); err != nil {
		return err
	}
	if err := jobFeature(p, e); err != nil {
		return err
	}
	if err := JobShape(e); err != nil {
		return err
	}
	if p.MaxFuture > 0 && e.CreatedAt > now+p.MaxFuture {
		return errors.New("invalid: event creation date is too far off from the current time")
	}
	if exp := event.Expiration(e); exp > 0 && exp <= now && e.Kind != event.KIND_AGENT_GRANT {
		return errors.New("invalid: event has already expired")
	}
	if g.cfg.Community != nil {
		if s.RemoteIP != "" {
			blocked, err := g.cfg.Community.IsIPBlocked(ctx, s.RemoteIP)
			if err != nil {
				return fmt.Errorf("check IP block: %w", err)
			}
			if blocked {
				return errors.New("blocked: this IP address is blocked")
			}
		}
		banned, err := g.cfg.Community.IsBanned(ctx, e.PubKey)
		if err != nil {
			return fmt.Errorf("check pubkey ban: %w", err)
		}
		if banned {
			return errors.New("blocked: this pubkey is banned")
		}
		banned, err = g.cfg.Community.IsEventBanned(ctx, e.ID)
		if err != nil {
			return fmt.Errorf("check event ban: %w", err)
		}
		if banned {
			return errors.New("blocked: this event is banned")
		}
	}
	if err := g.agentGrantShape(ctx, e, now); err != nil {
		return err
	}
	if err := g.agentAdmission(ctx, e, now); err != nil {
		return err
	}
	if where := blockedWord(p, e); where != "" && !g.isModerator(ctx, e.PubKey) {
		return errors.New("blocked: " + where)
	}
	if e.Kind == event.KIND_NOSTR_CONNECT {
		if !p.Features.Signer {
			return errors.New("restricted: signer traffic is switched off")
		}
		return nil
	}
	if err := g.roomWrite(ctx, e); err != nil {
		return err
	}
	if p.Inbox.Targeted && !inboxAdmission(p, e) {
		return errors.New("blocked: inbox event must target the relay owner")
	}
	if hasTag(e, "-") && !contains(s.PubKeys, e.PubKey) {
		return errors.New("auth-required: this event may only be published by its author")
	}
	writeAccess := accessFor(ctx, g.cfg.Community, p, relay.Session{PubKeys: []string{e.PubKey}})
	if p.Writes == "wot" && !writeAccess.Member && !writeAccess.Owner && !g.wotAllows(ctx, e.PubKey, p.Owner) {
		return errors.New("restricted: this relay only accepts events from its members and the people they follow")
	}
	if e.Kind == event.KIND_MARMOT_GROUP {
		principal, err := g.Principal(ctx, e, s)
		if err != nil {
			return err
		}
		if g.cfg.Community != nil {
			banned, banErr := g.cfg.Community.IsBanned(ctx, principal)
			if banErr != nil {
				return fmt.Errorf("check Marmot principal ban: %w", banErr)
			}
			if banned {
				return errors.New("blocked: Marmot account is banned")
			}
		}
		writeAccess = accessFor(ctx, g.cfg.Community, p, relay.Session{PubKeys: []string{principal}})
	}
	if err := jobWriter(e, writeAccess); err != nil {
		return err
	}
	if err := g.jobReply(ctx, e, now, false); err != nil {
		return err
	}
	ownerReplaceable := writeAccess.Owner && (event.IsReplaceable(e.Kind) || event.IsAddressable(e.Kind))
	allowed, blocked, rulesErr := g.kindRules(ctx, p, e.Kind)
	if rulesErr != nil {
		return fmt.Errorf("read kind rules: %w", rulesErr)
	}
	if blocked || !allowed {
		if !ownerReplaceable {
			return errors.New("blocked: this relay does not accept this event kind")
		}
	}
	membershipEvent := e.Kind == event.KIND_JOIN || e.Kind == event.KIND_LEAVE || e.Kind == event.KIND_NIP43_JOIN || e.Kind == event.KIND_NIP43_LEAVE
	if !ownerReplaceable && !membershipEvent && !policy.CanWrite(p, e, writeAccess) && !g.guestPass(ctx, p, e, writeAccess, now) {
		return errors.New("blocked: this event is not allowed by the relay write policy")
	}
	if days := g.RetentionDays(ctx, e.Kind); days > 0 && !protectedKind(e.Kind) && !(ownerReplaceable) && e.CreatedAt < now-int64(days)*86400 {
		return errors.New("blocked: event is older than this relay's retention window")
	}
	if e.Kind == event.KIND_WRAP && g.isDMInbox(ctx, p) && g.cfg.Community != nil && len(event.TagValues(e, "p")) > 0 {
		valid := false
		for _, recipient := range event.TagValues(e, "p") {
			member, err := g.cfg.Community.IsMember(ctx, recipient)
			if err == nil && member {
				valid = true
			}
		}
		if !valid {
			return errors.New("blocked: gift wrap recipient is not local")
		}
	}
	if p.MinPow > 0 && event.Difficulty(e.ID) < p.MinPow {
		return fmt.Errorf("pow: difficulty %d is less than %d", event.Difficulty(e.ID), p.MinPow)
	}
	if p.MinPow > 0 && event.CommittedDifficulty(e) > 0 && event.CommittedDifficulty(e) < p.MinPow {
		return fmt.Errorf("pow: committed target %d is less than %d", event.CommittedDifficulty(e), p.MinPow)
	}
	return nil
}

func inboxAdmission(p policy.Policy, e event.Event) bool {
	if e.PubKey == p.Owner && (e.Kind == 0 || e.Kind == 3 || e.Kind == 10002) {
		return true
	}
	for _, tag := range e.Tags {
		if len(tag) >= 2 && tag[0] == "p" && tag[1] == p.Owner {
			return true
		}
	}
	return false
}

// Import validates an event received from another host using protocol shape,
// safety, ban, agent and room existence checks. The caller records its origin
// in storage after this common gate succeeds.
func (g *Gate) Import(ctx context.Context, e event.Event, now int64) error {
	p := g.cfg.Policy()
	if err := event.Validate(e); err != nil {
		return err
	}
	if err := nip43Shape(e, now); err != nil {
		return err
	}
	if err := jobFeature(p, e); err != nil {
		return err
	}
	if err := JobShape(e); err != nil {
		return err
	}
	if p.MaxFuture > 0 && e.CreatedAt > now+p.MaxFuture {
		return errors.New("invalid: event creation date is too far off from the current time")
	}
	if exp := event.Expiration(e); exp > 0 && exp <= now && e.Kind != event.KIND_AGENT_GRANT {
		return errors.New("invalid: event has already expired")
	}
	if g.cfg.Community != nil {
		banned, err := g.cfg.Community.IsBanned(ctx, e.PubKey)
		if err != nil {
			return fmt.Errorf("check pubkey ban: %w", err)
		}
		if banned {
			return errors.New("blocked: this pubkey is banned")
		}
		banned, err = g.cfg.Community.IsEventBanned(ctx, e.ID)
		if err != nil {
			return fmt.Errorf("check event ban: %w", err)
		}
		if banned {
			return errors.New("blocked: this event is banned")
		}
	}
	if err := g.agentGrantShape(ctx, e, now); err != nil {
		return err
	}
	if err := g.agentAdmission(ctx, e, now); err != nil {
		return err
	}
	if where := blockedWord(p, e); where != "" && !g.isModerator(ctx, e.PubKey) {
		return errors.New("blocked: " + where)
	}
	if e.Kind == event.KIND_AUTH {
		return errors.New("blocked: kind 22242 is only accepted inside AUTH")
	}
	if e.Kind == event.KIND_NOSTR_CONNECT && !p.Features.Signer {
		return errors.New("restricted: signer traffic is switched off")
	}
	if e.Kind == event.KIND_MARMOT_GROUP || e.Kind == event.KIND_MARMOT_KEY_PACKAGE {
		if !p.Features.Marmot {
			return errors.New("restricted: Marmot transport is switched off on this relay")
		}
		if err := MarmotShape(e); err != nil {
			return err
		}
	}
	if err := g.roomExists(ctx, e); err != nil {
		return err
	}
	return nil
}

// roomID names the room an event belongs to, or nothing for events outside
// the room dialect. Marmot group messages reuse the h tag for other ends.
func (g *Gate) roomID(e event.Event) string {
	if e.Kind == event.KIND_MARMOT_GROUP {
		return ""
	}
	return event.Tag(e, "h")
}

// roomExists is the common room check: an h tag must name the slug room or
// a live room. Room creation is the one event allowed to name a new room.
func (g *Gate) roomExists(ctx context.Context, e event.Event) error {
	id := g.roomID(e)
	if id == "" || id == g.cfg.Slug || e.Kind == event.KIND_CREATE_GROUP {
		return nil
	}
	if g.cfg.Community == nil {
		return fmt.Errorf("blocked: this relay hosts one group: %s", g.cfg.Slug)
	}
	room, err := g.cfg.Community.Room(ctx, id)
	if err != nil {
		return fmt.Errorf("check room: %w", err)
	}
	if !room.Live() {
		return fmt.Errorf("invalid: unknown room %s", id)
	}
	return nil
}

// roomWrite applies room membership to client writes. The slug room keeps
// the tenant's own rules. In other rooms, management events are checked by
// the room handler; every other event needs room membership, which an open
// room grants to any tenant member.
func (g *Gate) roomWrite(ctx context.Context, e event.Event) error {
	if err := g.roomExists(ctx, e); err != nil {
		return err
	}
	id := g.roomID(e)
	if id != "" && community.RoomNoticeKind(e.Kind) {
		return errors.New("blocked: member notices are signed by the relay")
	}
	if id == "" || id == g.cfg.Slug || e.Kind == event.KIND_CREATE_GROUP || community.RoomAdminKind(e.Kind) {
		return nil
	}
	allowed, _, err := g.cfg.Community.RoomWriteAllowed(ctx, id, e.PubKey)
	if err != nil {
		return fmt.Errorf("check room membership: %w", err)
	}
	if !allowed {
		return fmt.Errorf("restricted: not a member of room %s", id)
	}
	return nil
}

// roomVisible applies room access to reads. Members-only rooms, including
// the relay's own 39000 to 39002 records for them, are served only to room
// members, the tenant owner and moderators.
func (g *Gate) roomVisible(ctx context.Context, e event.Event, s relay.Session) bool {
	id := g.roomID(e)
	if id == "" && (e.Kind == event.KIND_GROUP_METADATA || e.Kind == event.KIND_GROUP_ADMINS || e.Kind == event.KIND_GROUP_MEMBERS) {
		id = event.Tag(e, "d")
	}
	if id == "" || id == g.cfg.Slug || g.cfg.Community == nil {
		return true
	}
	visible, err := g.cfg.Community.RoomVisible(ctx, id, s.PubKeys)
	return err == nil && visible
}

// membersOnlyRoomFilter reports whether a filter asks for a members-only
// room, so an unauthenticated subscriber is told to AUTH instead of waiting
// on an empty stream.
func (g *Gate) membersOnlyRoomFilter(ctx context.Context, f event.Filter) bool {
	if g.cfg.Community == nil {
		return false
	}
	for _, id := range f.Tags["h"] {
		if id == g.cfg.Slug {
			continue
		}
		room, err := g.cfg.Community.Room(ctx, id)
		if err == nil && room.Live() && room.Access == community.RoomMembers {
			return true
		}
	}
	return false
}

func (g *Gate) kindRules(ctx context.Context, p policy.Policy, kind int) (bool, bool, error) {
	if g.cfg.Community == nil {
		return len(p.AllowedKinds) == 0 || containsInt(p.AllowedKinds, kind), containsInt(p.BlockedKinds, kind), nil
	}
	allowedAny, blocked := false, false
	value, err := g.cfg.Community.Execute(ctx, p.Owner, "listallowedkinds", nil)
	if err != nil {
		return false, false, err
	}
	if values, ok := value.([]int); ok {
		allowedAny = len(values) == 0 || containsInt(values, kind)
	} else {
		allowedAny = true
	}
	value, err = g.cfg.Community.Execute(ctx, p.Owner, "listblockedkinds", nil)
	if err != nil {
		return false, false, err
	}
	if values, ok := value.([]int); ok {
		blocked = containsInt(values, kind)
	}
	return allowedAny, blocked, nil
}

// RetentionDays returns the configured retention period for kind, using the
// wildcard rule when no exact rule exists.
func (g *Gate) RetentionDays(ctx context.Context, kind int) int {
	if g.cfg.Community == nil {
		wildcard := 0
		for _, rule := range g.cfg.Policy().Retention {
			if rule.Kind == kind {
				return rule.Days
			}
			if rule.Kind < 0 {
				wildcard = rule.Days
			}
		}
		return wildcard
	}
	value, err := g.cfg.Community.Execute(ctx, g.cfg.Policy().Owner, "listretention", nil)
	if err != nil {
		return 0
	}
	if rules, ok := value.([]map[string]int); ok {
		wildcard := 0
		for _, rule := range rules {
			if rule["kind"] == kind {
				return rule["days"]
			}
			if rule["kind"] < 0 {
				wildcard = rule["days"]
			}
		}
		return wildcard
	}
	return 0
}

func protectedKind(kind int) bool {
	switch kind {
	case 0, 3, 10002, 9735, 13534, 8000, 8001, 9000, 9001, 33534, 39000, 39001, 39002, 39003, 39005:
		return true
	}
	return false
}

func nip43Shape(e event.Event, now int64) error {
	if e.Kind != event.KIND_NIP43_JOIN && e.Kind != event.KIND_NIP43_LEAVE {
		return nil
	}
	if delta := e.CreatedAt - now; delta > 300 || delta < -300 {
		return errors.New("invalid: NIP-43 request is outside the allowed time window")
	}
	if e.Kind == event.KIND_NIP43_LEAVE {
		protected := 0
		for _, tag := range e.Tags {
			if len(tag) > 0 && tag[0] == "-" {
				protected++
				if len(tag) != 1 {
					return errors.New("invalid: NIP-43 leave protected tag must be empty")
				}
			}
		}
		if protected != 1 {
			return errors.New("invalid: NIP-43 leave needs one protected tag")
		}
	}
	// A join without a claim tag is an access request held for the owner's
	// review, so the claim stays optional.
	return nil
}

func (g *Gate) wotAllows(ctx context.Context, target, owner string) bool {
	if g.cfg.Store == nil {
		return false
	}
	principals := []string{owner}
	if g.cfg.Community != nil {
		members, err := g.cfg.Community.Execute(ctx, owner, "listmembers", nil)
		if err == nil {
			if list, ok := members.([]community.Member); ok {
				for _, member := range list {
					principals = append(principals, member.PubKey)
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, principal := range principals {
		if seen[principal] {
			continue
		}
		seen[principal] = true
		result, err := g.cfg.Store.Query(ctx, event.Filter{Authors: []string{principal}, Kinds: []int{3}}, storage.QueryOptions{Now: 0, Access: storage.Access{All: true}})
		if err != nil || len(result.Events) == 0 {
			continue
		}
		latest := result.Events[0]
		for _, tag := range latest.Tags {
			if len(tag) > 1 && tag[0] == "p" && tag[1] == target {
				return true
			}
		}
	}
	return false
}

func (g *Gate) isDMInbox(ctx context.Context, p policy.Policy) bool {
	if p.Reads != "members" || g.cfg.Community == nil {
		return p.Reads == "members" && containsInt(p.AllowedKinds, event.KIND_WRAP)
	}
	value, err := g.cfg.Community.Execute(ctx, p.Owner, "listallowedkinds", nil)
	if err != nil {
		return false
	}
	values, ok := value.([]int)
	return ok && containsInt(values, event.KIND_WRAP)
}

func (g *Gate) guestPass(ctx context.Context, p policy.Policy, e event.Event, a policy.Access, now int64) bool {
	if !a.Member && !a.Owner && p.Writes != "open" {
		if containsInt(p.OpenKinds, e.Kind) {
			return true
		}
		if !p.GuestReplies || (e.Kind != 1 && e.Kind != 1111) || g.cfg.Store == nil {
			return false
		}
		for _, id := range append(event.TagValues(e, "e"), event.TagValues(e, "E")...) {
			result, err := g.cfg.Store.Query(ctx, event.Filter{IDs: []string{id}}, storage.QueryOptions{Now: now, Access: storage.Access{All: true}, Limit: 1})
			if err == nil && len(result.Events) > 0 {
				author := result.Events[0].PubKey
				if author == p.Owner {
					return true
				}
				if g.cfg.Community != nil {
					member, memberErr := g.cfg.Community.IsMember(ctx, author)
					if memberErr == nil && member {
						return true
					}
				}
			}
		}
	}
	return false
}

// Read validates a subscription request and returns an AUTH hint when a
// subscriber may receive private or members-only results after authenticating.
func (g *Gate) Read(ctx context.Context, filters []event.Filter, s relay.Session) (bool, error) {
	p := g.cfg.Policy()
	if p.Features.Signer && len(filters) > 0 {
		signerOnly := true
		for _, f := range filters {
			if len(f.Kinds) != 1 || f.Kinds[0] != event.KIND_NOSTR_CONNECT {
				signerOnly = false
			}
		}
		if signerOnly {
			return false, nil
		}
	}
	if p.Features.Search == "off" {
		for _, f := range filters {
			if f.Search != "" {
				return false, errors.New("unsupported: search is switched off on this relay")
			}
		}
	}
	a := accessFor(ctx, g.cfg.Community, p, s)
	if p.Reads == "auth" && len(a.PubKeys) == 0 {
		return false, errors.New("auth-required: this relay requires AUTH")
	}
	if p.Reads == "members" && !a.Member && !a.Owner {
		return false, errors.New("auth-required: this relay is members-only")
	}
	privateOnly := true
	authHint := false
	for _, f := range filters {
		if len(a.PubKeys) == 0 && g.membersOnlyRoomFilter(ctx, f) {
			authHint = true
		}
		if len(f.Kinds) == 0 {
			privateOnly = false
			authHint = true
			continue
		}
		seenPrivate := false
		for _, k := range f.Kinds {
			if event.IsPrivate(k) {
				seenPrivate = true
			} else {
				privateOnly = false
			}
		}
		if seenPrivate && len(a.PubKeys) == 0 {
			authHint = true
		}
	}
	if privateOnly && len(a.PubKeys) == 0 {
		return false, errors.New("auth-required: private kinds are only served to their recipients")
	}
	return authHint, nil
}

// CanSee reports whether session s may receive e. It applies hidden-event,
// ban, repository privacy, policy, room and recipient checks.
func (g *Gate) CanSee(ctx context.Context, e event.Event, s relay.Session, f *event.Filter) bool {
	p := g.cfg.Policy()
	if community.IsAgentGrantRequest(e) {
		operator := event.Tag(e, "p")
		if !contains(s.PubKeys, e.PubKey) && !contains(s.PubKeys, operator) {
			return false
		}
	}
	if g.cfg.Store != nil {
		var held int
		if err := g.cfg.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT id FROM hidden_events WHERE id=? UNION ALL SELECT id FROM pending_events WHERE id=?)`, e.ID, e.ID).Scan(&held); err != nil || held > 0 {
			return false
		}
	}
	private, err := privateRepositoryEvent(ctx, g.cfg.Store, e)
	if err != nil || private && (!p.Features.Grasp08 || p.Reads != "members") {
		return false
	}
	if g.cfg.Community != nil {
		banned, err := g.cfg.Community.IsEventBanned(ctx, e.ID)
		if err != nil || banned {
			return false
		}
		banned, err = g.cfg.Community.IsBanned(ctx, e.PubKey)
		if err != nil || banned {
			return false
		}
		for _, key := range s.PubKeys {
			banned, err = g.cfg.Community.IsBanned(ctx, key)
			if err != nil || banned {
				return false
			}
		}
	}
	if e.Kind == event.KIND_PUSH_REGISTRATION {
		return contains(s.PubKeys, e.PubKey)
	}
	if e.Kind == event.KIND_NOSTR_CONNECT {
		return p.Features.Signer && canSeeSignerMessage(e, s, f)
	}
	a := accessFor(ctx, g.cfg.Community, p, s)
	if !policy.CanRead(p, e, a) {
		return false
	}
	if !g.roomVisible(ctx, e, s) {
		return false
	}
	if event.IsPrivate(e.Kind) {
		for _, key := range s.PubKeys {
			if key == e.PubKey {
				return true
			}
		}
		for _, tag := range e.Tags {
			if len(tag) >= 2 && tag[0] == "p" && contains(s.PubKeys, tag[1]) {
				return true
			}
		}
		return false
	}
	return true
}

// privateRepositoryEvent also protects legacy state events. Their privacy is
// inherited from the corresponding replaceable announcement, so checking only
// the state event's own tags would allow metadata to escape after a restart.
func privateRepositoryEvent(ctx context.Context, store *storage.Store, e event.Event) (bool, error) {
	return policy.PrivateRepository(ctx, store, e)
}

func canSeeSignerMessage(e event.Event, s relay.Session, f *event.Filter) bool {
	if contains(s.PubKeys, e.PubKey) {
		return true
	}
	for _, recipient := range event.TagValues(e, "p") {
		if contains(s.PubKeys, recipient) {
			return true
		}
		// The client must receive encrypted replies before it can use its
		// remote signer to authenticate. Limit this to its addressed inbox.
		if f != nil && len(f.Kinds) == 1 && f.Kinds[0] == event.KIND_NOSTR_CONNECT && contains(f.Tags["p"], recipient) {
			return true
		}
	}
	return false
}

func accessFor(ctx context.Context, c *community.Service, p policy.Policy, s relay.Session) policy.Access {
	a := policy.Access{PubKeys: append([]string(nil), s.PubKeys...)}
	a.Owner = contains(s.PubKeys, p.Owner)
	if c != nil {
		for _, key := range s.PubKeys {
			member, err := c.IsMember(ctx, key)
			if err == nil && member {
				a.Member = true
			}
			role, err := c.Role(ctx, key)
			if err == nil && role != "" {
				a.Role = role
			}
		}
	}
	return a
}

func (g *Gate) isModerator(ctx context.Context, key string) bool {
	if g.cfg.Community == nil {
		return false
	}
	role, err := g.cfg.Community.Role(ctx, key)
	return err == nil && (role == "owner" || role == "moderator")
}

func blockedWord(p policy.Policy, e event.Event) string {
	for _, word := range p.BlockedWords {
		pattern := word
		regex := false
		if strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/") {
			pattern, regex = pattern[1:len(pattern)-1], true
		}
		matches := func(value string) bool {
			if !regex {
				return strings.Contains(strings.ToLower(value), strings.ToLower(pattern))
			}
			re, err := regexp2.Compile(pattern, regexp2.IgnoreCase)
			if err != nil {
				return false
			}
			ok, err := re.MatchString(value)
			return err == nil && ok
		}
		if matches(e.Content) {
			return "content contains a blocked word"
		}
		if p.BlockedWordsInTags {
			for _, tag := range e.Tags {
				for _, value := range tag {
					if matches(value) {
						return "a tag contains a blocked word"
					}
				}
			}
		}
	}
	return ""
}

func hasTag(e event.Event, name string) bool {
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == name {
			return true
		}
	}
	return false
}
func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
func proofOfWork(id string) int {
	bits := 0
	for _, ch := range id {
		value := byte(0)
		switch {
		case ch >= '0' && ch <= '9':
			value = byte(ch - '0')
		case ch >= 'a' && ch <= 'f':
			value = byte(ch - 'a' + 10)
		default:
			return bits
		}
		for mask := byte(8); mask > 0 && value&mask == 0; mask >>= 1 {
			bits++
		}
		if value != 0 {
			break
		}
	}
	return bits
}
