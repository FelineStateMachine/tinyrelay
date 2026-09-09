// Package community owns tenant membership, moderation, invitations and audit.
package community

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const schema = `
CREATE TABLE IF NOT EXISTS community_members(pubkey TEXT PRIMARY KEY,name TEXT NOT NULL DEFAULT '',note TEXT NOT NULL DEFAULT '',role TEXT NOT NULL,invited_by TEXT NOT NULL DEFAULT '',via TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,keep_days INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS community_member_names ON community_members(name) WHERE name<>'';
CREATE TABLE IF NOT EXISTS community_bans(pubkey TEXT PRIMARY KEY,reason TEXT NOT NULL,at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS community_event_bans(id TEXT PRIMARY KEY,reason TEXT NOT NULL,at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS community_ip_blocks(ip TEXT PRIMARY KEY,reason TEXT NOT NULL,at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS community_retention(kind INTEGER PRIMARY KEY,days INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS community_kind_rules(kind INTEGER PRIMARY KEY,rule TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS community_invites(code TEXT PRIMARY KEY,created_by TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,max_uses INTEGER NOT NULL,uses INTEGER NOT NULL DEFAULT 0,note TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS community_reports(id INTEGER PRIMARY KEY AUTOINCREMENT,reporter TEXT NOT NULL,target_event TEXT NOT NULL,type TEXT NOT NULL,content TEXT NOT NULL,at INTEGER NOT NULL,status TEXT NOT NULL DEFAULT 'open',resolved_by TEXT NOT NULL DEFAULT '',resolved_at INTEGER NOT NULL DEFAULT 0,action TEXT NOT NULL DEFAULT '',UNIQUE(reporter,target_event));
CREATE TABLE IF NOT EXISTS community_audit(id INTEGER PRIMARY KEY AUTOINCREMENT,at INTEGER NOT NULL,actor TEXT NOT NULL,action TEXT NOT NULL,target TEXT NOT NULL DEFAULT '',detail TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS community_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TRIGGER IF NOT EXISTS community_member_projection_insert AFTER INSERT ON community_members
BEGIN
 INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at)
 VALUES('community-member-'||NEW.pubkey||'-'||NEW.created_at,'records-projection','community-member-'||NEW.pubkey||'-'||NEW.created_at,'membership',json_object('action','membership','pubkey',NEW.pubkey,'role',NEW.role),strftime('%s','now'),strftime('%s','now'),strftime('%s','now'));
END;
CREATE TRIGGER IF NOT EXISTS community_member_projection_update AFTER UPDATE OF name,note,role,invited_by,via,keep_days ON community_members
BEGIN
 INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at)
 VALUES('community-member-update-'||lower(hex(randomblob(16))),'records-projection','community-member-update-'||lower(hex(randomblob(16))),'membership',json_object('action','membership','pubkey',NEW.pubkey,'role',NEW.role),strftime('%s','now'),strftime('%s','now'),strftime('%s','now'));
END;
CREATE TRIGGER IF NOT EXISTS community_member_projection_delete AFTER DELETE ON community_members
BEGIN
 INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at)
 VALUES('community-member-delete-'||lower(hex(randomblob(16))),'records-projection','community-member-delete-'||lower(hex(randomblob(16))),'membership',json_object('action','membership','pubkey',OLD.pubkey,'role',OLD.role,'removed',1),strftime('%s','now'),strftime('%s','now'),strftime('%s','now'));
END;
`

type Member struct {
	PubKey    string `json:"pubkey"`
	Name      string `json:"name"`
	Note      string `json:"note"`
	Role      string `json:"role"`
	InvitedBy string `json:"invited_by"`
	Via       string `json:"via"`
	CreatedAt int64  `json:"created_at"`
	KeepDays  int    `json:"keep_days"`
}
type Invite struct {
	Code      string `json:"code"`
	CreatedBy string `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	MaxUses   int    `json:"max_uses"`
	Uses      int    `json:"uses"`
	Note      string `json:"note"`
}
type Report struct {
	ID          int64  `json:"id"`
	Reporter    string `json:"reporter"`
	TargetEvent string `json:"target_event"`
	Type        string `json:"type"`
	Content     string `json:"content"`
	At          int64  `json:"at"`
	Status      string `json:"status"`
	ResolvedBy  string `json:"resolved_by"`
	ResolvedAt  int64  `json:"resolved_at"`
	Action      string `json:"action"`
}
type AuditRow struct {
	ID     int64  `json:"id"`
	At     int64  `json:"at"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

type Service struct {
	store  *storage.Store
	mu     sync.RWMutex
	owner  string
	slug   string
	policy func() policy.Policy
}

func New(ctx context.Context, store *storage.Store, owner string) (*Service, error) {
	if store == nil {
		return nil, errors.New("community: nil store")
	}
	if !validPubKey(owner) {
		return nil, errors.New("community: invalid owner pubkey")
	}
	s := &Service{store: store, owner: owner}
	if _, err := store.DB().ExecContext(ctx, schema+agentSchema); err != nil {
		return nil, fmt.Errorf("community schema: %w", err)
	}
	if _, err := store.DB().ExecContext(ctx, roomSchema); err != nil {
		return nil, fmt.Errorf("room schema: %w", err)
	}
	if err := store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO community_members(pubkey,role,created_at) VALUES(?,?,?)`, owner, "owner", time.Now().Unix())
		return err
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// ConfigurePolicy supplies the current durable tenant policy. It is read at
// admission time so invitation depth, quota, and terms changes take effect
// without duplicating policy state in the community database.
func (s *Service) ConfigurePolicy(getter func() policy.Policy) {
	s.mu.Lock()
	s.policy = getter
	s.mu.Unlock()
}

func (s *Service) currentPolicy() policy.Policy {
	s.mu.RLock()
	getter := s.policy
	s.mu.RUnlock()
	if getter == nil {
		return policy.Policy{}
	}
	return getter()
}

// ApplyOwnerTx changes the community owner rows in a caller-owned
// transaction. The root must persist the corresponding policy owner in this
// same transaction, then call SetOwner after commit.
func (s *Service) ApplyOwnerTx(ctx context.Context, tx *sql.Tx, oldOwner, newOwner string) error {
	if !validPubKey(newOwner) || newOwner == oldOwner {
		return errors.New("invalid: new owner pubkey")
	}
	if oldOwner == "" {
		s.mu.RLock()
		oldOwner = s.owner
		s.mu.RUnlock()
	}
	var role string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM community_members WHERE pubkey=?`, newOwner).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("invalid: new owner must be a member")
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE community_members SET role='moderator' WHERE pubkey=? AND role='owner'`, oldOwner); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE community_members SET role='owner' WHERE pubkey=?`, newOwner); err != nil {
		return err
	}
	return nil
}

// SetOwner updates only the in-memory owner cache and is intended to run
// after the transaction that persists policy and membership rows commits.
func (s *Service) SetOwner(newOwner string) error {
	if !validPubKey(newOwner) {
		return errors.New("invalid: owner pubkey")
	}
	s.mu.Lock()
	s.owner = newOwner
	s.mu.Unlock()
	return nil
}

func (s *Service) Role(ctx context.Context, pubkey string) (string, error) {
	s.mu.RLock()
	owner := s.owner
	s.mu.RUnlock()
	if pubkey == owner {
		return "owner", nil
	}
	var role string
	err := s.store.DB().QueryRowContext(ctx, `SELECT role FROM community_members WHERE pubkey=?`, pubkey).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("role: %w", err)
	}
	return role, nil
}

// Moderators lists the keys that hold the moderator role.
func (s *Service) Moderators(ctx context.Context) ([]string, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey FROM community_members WHERE role='moderator' ORDER BY pubkey`)
	if err != nil {
		return nil, fmt.Errorf("moderators: %w", err)
	}
	defer rows.Close()
	var moderators []string
	for rows.Next() {
		var pubkey string
		if err := rows.Scan(&pubkey); err != nil {
			return nil, err
		}
		moderators = append(moderators, pubkey)
	}
	return moderators, rows.Err()
}

func (s *Service) Execute(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
	if method == "supportedmethods" {
		return s.Methods(), nil
	}
	role, err := s.Role(ctx, actor)
	if err != nil {
		return nil, err
	}
	if !allowed(method, role) {
		return nil, fmt.Errorf("restricted: %s may not %s", roleOrGuest(role), method)
	}
	switch method {
	case "setmember", "allowpubkey":
		return s.setMember(ctx, actor, params)
	case "removemember", "unrulepubkey":
		return s.removeMember(ctx, actor, params)
	case "removesubtree":
		return s.removeSubtree(ctx, actor, params)
	case "listmembers", "listpeople":
		return s.members(ctx)
	case "listallowedpubkeys":
		return s.allowedMembers(ctx)
	case "createinvite":
		return s.createInvite(ctx, actor, params, false)
	case "createclaim":
		return s.createInvite(ctx, actor, params, true)
	case "listinvites":
		return s.invites(ctx, actor, role)
	case "listclaims":
		return s.claims(ctx, actor, role)
	case "revokeinvite", "deleteclaim":
		return s.revokeInvite(ctx, actor, role, params)
	case "transferowner":
		return s.transferOwner(ctx, params)
	case "banpubkey":
		return s.ban(ctx, actor, params)
	case "listbannedpubkeys":
		return s.listBans(ctx)
	case "banevent":
		return s.banEvent(ctx, actor, params)
	case "listbannedevents":
		return s.listBannedEvents(ctx)
	case "blockip":
		return s.blockIP(ctx, actor, params, true)
	case "unblockip":
		return s.blockIP(ctx, actor, params, false)
	case "listblockedips":
		return s.listIPs(ctx)
	case "listaudit":
		return s.audit(ctx)
	case "listreports":
		return s.reports(ctx, params)
	case "listeventsneedingmoderation":
		return s.moderationQueue(ctx)
	case "setblockedwords":
		return s.setBlockedWords(ctx, actor, params)
	case "purgekind":
		return s.purgeKind(ctx, actor, params)
	case "setretention":
		return s.setRetention(ctx, actor, params)
	case "listretention":
		return s.listRetention(ctx)
	case "resolvereport":
		return s.resolveReport(ctx, actor, params)
	case "allowevent":
		return s.allowEvent(ctx, actor, params)
	case "allowkind", "disallowkind", "unrulekind":
		return s.setKind(ctx, actor, method, params)
	case "listallowedkinds", "listblockedkinds":
		rule := "block"
		if method == "listallowedkinds" {
			rule = "allow"
		}
		return s.listKinds(ctx, rule)
	default:
		return nil, fmt.Errorf("unsupported: unknown community method %q", method)
	}
}

func allowed(method, role string) bool {
	if method == "listmembers" || method == "listpeople" || method == "listallowedpubkeys" || method == "listbannedpubkeys" || method == "listbannedevents" || method == "listblockedips" || method == "listreports" || method == "listeventsneedingmoderation" || method == "listallowedkinds" || method == "listblockedkinds" || method == "listretention" {
		return role == "owner" || role == "moderator"
	}
	if role == "owner" {
		return true
	}
	if role == "member" {
		for _, name := range []string{"createinvite", "createclaim", "listinvites", "listclaims", "revokeinvite", "deleteclaim"} {
			if method == name {
				return true
			}
		}
		return false
	}
	if role != "moderator" {
		return false
	}
	for _, name := range []string{"setmember", "allowpubkey", "removemember", "unrulepubkey", "removesubtree", "createinvite", "createclaim", "listinvites", "listclaims", "revokeinvite", "deleteclaim", "banpubkey", "banevent", "allowevent", "blockip", "unblockip", "listaudit", "resolvereport", "allowkind", "disallowkind", "unrulekind", "setblockedwords"} {
		if method == name {
			return true
		}
	}
	return false
}

func (s *Service) Methods() []string {
	return []string{"supportedmethods", "listaudit", "listmembers", "listpeople", "listallowedpubkeys", "listbannedpubkeys", "setmember", "allowpubkey", "removemember", "unrulepubkey", "removesubtree", "createinvite", "listinvites", "revokeinvite", "listclaims", "createclaim", "deleteclaim", "banpubkey", "banevent", "allowevent", "listbannedevents", "blockip", "unblockip", "listblockedips", "listreports", "listeventsneedingmoderation", "resolvereport", "allowkind", "disallowkind", "unrulekind", "listallowedkinds", "listblockedkinds", "setretention", "listretention", "purgekind", "setblockedwords"}
}

func roleOrGuest(role string) string {
	if role == "" {
		return "guest"
	}
	return role
}

func validPubKey(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > 'f' || c > '9' && c < 'a' {
			return false
		}
	}
	return true
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
var codeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)

func decode[T any](params []json.RawMessage, index int, out *T) error {
	if index >= len(params) {
		return errors.New("invalid: missing parameter")
	}
	if err := json.Unmarshal(params[index], out); err != nil {
		return fmt.Errorf("invalid: parameter %d: %w", index, err)
	}
	return nil
}
func now() int64 { return time.Now().Unix() }
func token() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
