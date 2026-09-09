package community

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

func (s *Service) setMember(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	var pk, name, role string
	if err := decode(p, 0, &pk); err != nil {
		return nil, err
	}
	if !validPubKey(pk) {
		return nil, errors.New("invalid: bad pubkey")
	}
	var patch struct {
		Name *string `json:"name"`
		Note string  `json:"note"`
		Role string  `json:"role"`
	}
	if len(p) > 1 {
		if actor != s.owner {
			var targetRole string
			err := s.store.DB().QueryRowContext(ctx, `SELECT role FROM community_members WHERE pubkey=?`, pk).Scan(&targetRole)
			if err == nil && targetRole != "member" {
				return nil, errors.New("restricted: moderators cannot edit the owner or other moderators")
			}
		}
		raw := strings.TrimSpace(string(p[1]))
		if len(raw) > 0 && raw[0] == '"' {
			if err := json.Unmarshal(p[1], &patch.Note); err != nil {
				return nil, err
			}
		} else if err := json.Unmarshal(p[1], &patch); err != nil {
			return nil, fmt.Errorf("invalid: member patch: %w", err)
		}
	}
	if patch.Name != nil {
		name = *patch.Name
	}
	role = patch.Role
	if role == "" {
		role = "member"
	}
	if role != "member" && role != "moderator" {
		return nil, errors.New("invalid: role must be member or moderator")
	}
	if name != "" {
		name = strings.ToLower(strings.TrimSpace(name))
		if !nameRE.MatchString(name) {
			return nil, errors.New("invalid: bad member name")
		}
	}
	if role == "moderator" && actor != s.owner {
		return nil, errors.New("restricted: only the owner may appoint moderators")
	}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM community_bans WHERE pubkey=?`, pk); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO community_members(pubkey,name,role,created_at,via) VALUES(?,?,?,?,?) ON CONFLICT(pubkey) DO UPDATE SET name=excluded.name,role=excluded.role`, pk, name, role, now(), "management")
		if err != nil {
			return fmt.Errorf("set member: %w", err)
		}
		if patch.Note != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE community_members SET note=? WHERE pubkey=?`, patch.Note[:min(200, len(patch.Note))], pk); err != nil {
				return err
			}
		}
		return s.recordTx(ctx, tx, actor, "setmember", pk, role)
	})
	return map[string]any{"pubkey": pk, "role": role}, err
}

func (s *Service) removeMember(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	var pk string
	if err := decode(p, 0, &pk); err != nil {
		return nil, err
	}
	if !validPubKey(pk) {
		return nil, errors.New("invalid: bad pubkey")
	}
	if pk == s.owner {
		return nil, errors.New("invalid: cannot remove owner")
	}
	if actor != s.owner {
		var targetRole string
		if err := s.store.DB().QueryRowContext(ctx, `SELECT role FROM community_members WHERE pubkey=?`, pk).Scan(&targetRole); err == nil && targetRole == "moderator" {
			return nil, errors.New("restricted: moderators cannot remove the owner or other moderators")
		}
	}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM community_members WHERE pubkey=?`, pk); err != nil {
			return err
		}
		return s.recordTx(ctx, tx, actor, "removemember", pk, "")
	})
	return map[string]any{"removed": pk}, err
}

func (s *Service) removeSubtree(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	var root string
	if err := decode(p, 0, &root); err != nil {
		return nil, err
	}
	if !validPubKey(root) {
		return nil, errors.New("invalid: bad pubkey")
	}
	if root == s.owner {
		return nil, errors.New("invalid: cannot remove the owner")
	}
	role, _ := s.Role(ctx, root)
	if role != "member" {
		return nil, errors.New("restricted: moderators cannot remove the owner or other moderators")
	}
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey FROM community_members WHERE invited_by=? AND role='member'`, parent)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var pk string
			if err := rows.Scan(&pk); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[pk] {
				seen[pk] = true
				queue = append(queue, pk)
			}
		}
		rows.Close()
	}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		for pk := range seen {
			if _, err := tx.ExecContext(ctx, `DELETE FROM community_members WHERE pubkey=?`, pk); err != nil {
				return err
			}
		}
		return s.recordTx(ctx, tx, actor, "removesubtree", root, fmt.Sprint(len(seen)))
	})
	return map[string]any{"removed": len(seen)}, err
}

func (s *Service) members(ctx context.Context) (any, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,name,note,role,invited_by,via,created_at,keep_days FROM community_members ORDER BY created_at,pubkey`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.PubKey, &m.Name, &m.Note, &m.Role, &m.InvitedBy, &m.Via, &m.CreatedAt, &m.KeepDays); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return map[string]any{"self": s.owner, "members": out}, rows.Err()
}

func (s *Service) allowedMembers(ctx context.Context) (any, error) {
	value, err := s.members(ctx)
	if err != nil {
		return nil, err
	}
	members := value.(map[string]any)["members"].([]Member)
	out := make([]map[string]string, 0, len(members))
	for _, member := range members {
		if member.Role != "owner" {
			out = append(out, map[string]string{"pubkey": member.PubKey, "reason": member.Note})
		}
	}
	return out, nil
}

func (s *Service) createInvite(ctx context.Context, actor string, p []json.RawMessage, claim bool) (any, error) {
	code := ""
	if claim && len(p) != 1 {
		return nil, errors.New("invalid: give one invite code of 4 to 64 letters, digits, dash or underscore")
	}
	if claim && len(p) > 0 {
		if err := decode(p, 0, &code); err != nil {
			return nil, err
		}
	}
	if code != "" && !codeRE.MatchString(code) {
		return nil, errors.New("invalid: invite code must be 4 to 64 letters, digits, dash or underscore")
	}
	if !claim && code == "" {
		var err error
		code, err = token()
		if err != nil {
			return nil, err
		}
	}
	if role, _ := s.Role(ctx, actor); role == "member" {
		if reason := s.memberInviteGate(ctx, actor); reason != "" {
			return nil, errors.New(reason)
		}
	}
	ttl, maxUses := int64(3*86400), 0
	note := ""
	if !claim {
		if len(p) > 0 {
			_ = decode(p, 0, &ttl)
		}
		if len(p) > 1 {
			_ = decode(p, 1, &maxUses)
		}
		if len(p) > 2 {
			_ = decode(p, 2, &note)
			if len(note) > 200 {
				note = note[:200]
			}
		}
	}
	if claim && !codeRE.MatchString(code) {
		return nil, errors.New("invalid: give one invite code of 4 to 64 letters, digits, dash or underscore")
	}
	if !claim && ttl < 60 {
		ttl = 60
	}
	if !claim && ttl > 30*86400 {
		ttl = 30 * 86400
	}
	if maxUses < 0 {
		maxUses = 0
	}
	inv := Invite{Code: code, CreatedBy: actor, CreatedAt: now(), ExpiresAt: now() + ttl, MaxUses: maxUses, Note: note}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO community_invites(code,created_by,created_at,expires_at,max_uses,note) VALUES(?,?,?,?,?,?)`, inv.Code, inv.CreatedBy, inv.CreatedAt, inv.ExpiresAt, inv.MaxUses, inv.Note)
		if err != nil {
			return fmt.Errorf("create invite: %w", err)
		}
		return s.recordTx(ctx, tx, actor, "createinvite", code, "")
	})
	if claim {
		if err != nil {
			return nil, err
		}
		return true, nil
	}
	return inv, err
}

func (s *Service) Claim(ctx context.Context, code, pubkey string) (any, error) {
	return s.ClaimWithTerms(ctx, code, pubkey, "")
}

// IssueNIP43Invite creates a short-lived claim for an authenticated member.
// The relay root signs the resulting kind 28935 response; this package owns
// claim policy and persistence but never signs relay events itself.
func (s *Service) IssueNIP43Invite(ctx context.Context, requester string) (Invite, error) {
	if !validPubKey(requester) {
		return Invite{}, errors.New("invalid: invite requester pubkey")
	}
	role, err := s.Role(ctx, requester)
	if err != nil {
		return Invite{}, err
	}
	if role == "" {
		return Invite{}, errors.New("restricted: invite request requires membership")
	}
	value, err := s.createInvite(ctx, requester, nil, false)
	if err != nil {
		return Invite{}, err
	}
	invite, ok := value.(Invite)
	if !ok {
		return Invite{}, errors.New("community: invalid invite result")
	}
	return invite, nil
}

// ClaimWithTerms joins through an invite. When the relay publishes join terms,
// consent must include the SHA-256 hash of the exact terms text.
func (s *Service) ClaimWithTerms(ctx context.Context, code, pubkey, termsHash string) (any, error) {
	if !validPubKey(pubkey) {
		return nil, errors.New("invalid: bad pubkey")
	}
	banned, err := s.IsBanned(ctx, pubkey)
	if err != nil {
		return nil, err
	}
	if banned {
		return nil, errors.New("blocked: this pubkey is banned from this relay")
	}
	terms := s.currentPolicy().JoinTerms
	if terms != "" {
		sum := sha256.Sum256([]byte(terms))
		if !strings.EqualFold(hex.EncodeToString(sum[:]), termsHash) {
			return nil, errors.New("terms_mismatch")
		}
	}
	var member bool
	if err := s.store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM community_members WHERE pubkey=?)`, pubkey).Scan(&member); err != nil {
		return nil, err
	}
	if member {
		role, _ := s.Role(ctx, pubkey)
		return map[string]any{"status": "already_member", "role": role}, nil
	}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var id, creator string
		var exp int64
		var max, uses int
		err := tx.QueryRowContext(ctx, `SELECT code,created_by,expires_at,max_uses,uses FROM community_invites WHERE code=?`, code).Scan(&id, &creator, &exp, &max, &uses)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("invite_invalid")
		}
		if err != nil {
			return err
		}
		if exp < now() {
			return errors.New("invite_expired")
		}
		if max > 0 && uses >= max {
			return errors.New("invite_exhausted")
		}
		var creatorExists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM community_members WHERE pubkey=?)`, creator).Scan(&creatorExists); err != nil {
			return err
		}
		if !creatorExists {
			return errors.New("invite_revoked")
		}
		r, err := tx.ExecContext(ctx, `UPDATE community_invites SET uses=uses+1 WHERE code=? AND (max_uses=0 OR uses<max_uses)`, code)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return errors.New("invite_exhausted")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO community_members(pubkey,role,invited_by,via,created_at) VALUES(?,?,?,?,?)`, pubkey, "member", creator, "invite "+code, now()); err != nil {
			return err
		}
		return s.recordTx(ctx, tx, pubkey, "claiminvite", code, "")
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "joined", "role": "member"}, nil
}

func (s *Service) memberInviteGate(ctx context.Context, pubkey string) string {
	p := s.currentPolicy().MemberInvites
	if p.Depth <= 0 || p.Quota <= 0 {
		return "restricted: only the owner and moderators mint invites here"
	}
	depth := s.inviteDepth(ctx, pubkey)
	if depth >= p.Depth {
		return "restricted: invites do not reach this far down the tree"
	}
	var active int
	if err := s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM community_invites WHERE created_by=? AND expires_at>=? AND (max_uses=0 OR uses<max_uses)`, pubkey, now()).Scan(&active); err != nil {
		return "restricted: invite policy unavailable"
	}
	if active >= p.Quota {
		return fmt.Sprintf("restricted: you already hold %d live invite%s", p.Quota, map[bool]string{true: "", false: "s"}[p.Quota == 1])
	}
	return ""
}

func (s *Service) memberInviteGateTx(ctx context.Context, tx *sql.Tx, pubkey string) string {
	p := s.currentPolicy().MemberInvites
	if p.Depth <= 0 || p.Quota <= 0 {
		return "restricted: only the owner and moderators mint invites here"
	}
	depth := 0
	cur := pubkey
	seen := map[string]bool{cur: true}
	for depth < 100 {
		var parent string
		if err := tx.QueryRowContext(ctx, `SELECT invited_by FROM community_members WHERE pubkey=?`, cur).Scan(&parent); err != nil || parent == "" {
			break
		}
		depth++
		if parent == s.owner || seen[parent] {
			break
		}
		seen[parent] = true
		cur = parent
	}
	if depth >= p.Depth {
		return "restricted: invites do not reach this far down the tree"
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM community_invites WHERE created_by=? AND expires_at>=? AND (max_uses=0 OR uses<max_uses)`, pubkey, now()).Scan(&active); err != nil {
		return "restricted: invite policy unavailable"
	}
	if active >= p.Quota {
		return fmt.Sprintf("restricted: you already hold %d live invite%s", p.Quota, map[bool]string{true: "", false: "s"}[p.Quota == 1])
	}
	return ""
}

func (s *Service) inviteDepth(ctx context.Context, pubkey string) int {
	depth := 0
	seen := map[string]bool{pubkey: true}
	cur := pubkey
	for depth < 100 {
		var parent string
		if err := s.store.DB().QueryRowContext(ctx, `SELECT invited_by FROM community_members WHERE pubkey=?`, cur).Scan(&parent); err != nil || parent == "" {
			return depth
		}
		depth++
		if parent == s.owner || seen[parent] {
			return depth
		}
		seen[parent] = true
		cur = parent
	}
	return depth
}

func (s *Service) invites(ctx context.Context, actor, role string) (any, error) {
	query := `SELECT code,created_by,created_at,expires_at,max_uses,uses,note FROM community_invites`
	args := []any{}
	if role == "member" {
		query += ` WHERE created_by=?`
		args = append(args, actor)
	}
	query += ` ORDER BY created_at DESC LIMIT 200`
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Invite{}
	for rows.Next() {
		var i Invite
		if err := rows.Scan(&i.Code, &i.CreatedBy, &i.CreatedAt, &i.ExpiresAt, &i.MaxUses, &i.Uses, &i.Note); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func (s *Service) claims(ctx context.Context, actor, role string) (any, error) {
	q := `SELECT code FROM community_invites WHERE expires_at>=? AND (max_uses=0 OR uses<max_uses)`
	args := []any{now()}
	if role == "member" {
		q += ` AND created_by=?`
		args = append(args, actor)
	}
	rows, err := s.store.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Service) revokeInvite(ctx context.Context, actor, role string, p []json.RawMessage) (any, error) {
	var code string
	if err := decode(p, 0, &code); err != nil {
		return nil, err
	}
	if role == "member" {
		var by string
		if err := s.store.DB().QueryRowContext(ctx, `SELECT created_by FROM community_invites WHERE code=?`, code).Scan(&by); err != nil || by != actor {
			return nil, errors.New("restricted: not your invite")
		}
	}
	r, err := s.store.DB().ExecContext(ctx, `DELETE FROM community_invites WHERE code=?`, code)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	return map[string]any{"revoked": n == 1}, nil
}

func (s *Service) transferOwner(ctx context.Context, p []json.RawMessage) (any, error) {
	var pk string
	if err := decode(p, 0, &pk); err != nil {
		return nil, err
	}
	if !validPubKey(pk) {
		return nil, errors.New("invalid: bad pubkey")
	}
	old := s.owner
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error { return s.ApplyOwnerTx(ctx, tx, old, pk) })
	if err == nil {
		_ = s.SetOwner(pk)
	}
	return map[string]any{"owner": pk}, err
}

func (s *Service) ban(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	var pk, reason string
	if err := decode(p, 0, &pk); err != nil {
		return nil, err
	}
	if len(p) > 1 {
		_ = decode(p, 1, &reason)
	}
	if pk == s.owner {
		return nil, errors.New("invalid: cannot ban the owner")
	}
	if actor != s.owner {
		var targetRole string
		if err := s.store.DB().QueryRowContext(ctx, `SELECT role FROM community_members WHERE pubkey=?`, pk).Scan(&targetRole); err == nil && targetRole == "moderator" {
			return nil, errors.New("restricted: moderators cannot ban other moderators")
		}
	}
	if !validPubKey(pk) {
		return nil, errors.New("invalid: bad pubkey")
	}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO community_bans(pubkey,reason,at) VALUES(?,?,?) ON CONFLICT(pubkey) DO UPDATE SET reason=excluded.reason,at=excluded.at`, pk, reason, now()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM community_members WHERE pubkey=?`, pk); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM events WHERE pubkey=?`, pk)
		return err
	})
	return map[string]any{"banned": pk}, err
}
func (s *Service) listBans(ctx context.Context) (any, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT pubkey,reason,at FROM community_bans ORDER BY at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var p, r string
		var at int64
		if err := rows.Scan(&p, &r, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"pubkey": p, "reason": r, "at": at})
	}
	return out, rows.Err()
}
func (s *Service) banEvent(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	var id, reason string
	if err := decode(p, 0, &id); err != nil {
		return nil, err
	}
	if len(p) > 1 {
		_ = decode(p, 1, &reason)
	}
	_, err := s.store.DB().ExecContext(ctx, `INSERT INTO community_event_bans(id,reason,at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET reason=excluded.reason,at=excluded.at`, id, reason, now())
	return map[string]any{"banned": id}, err
}

func (s *Service) allowEvent(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	var id string
	if err := decode(p, 0, &id); err != nil {
		return nil, err
	}
	_, err := s.store.DB().ExecContext(ctx, `DELETE FROM community_event_bans WHERE id=?`, id)
	return true, err
}

func (s *Service) setKind(ctx context.Context, actor, method string, p []json.RawMessage) (any, error) {
	var kind int
	if err := decode(p, 0, &kind); err != nil {
		return nil, err
	}
	if kind < 0 || kind > 65535 {
		return nil, errors.New("invalid: kind out of range")
	}
	rule := ""
	if method == "allowkind" {
		rule = "allow"
	} else if method == "disallowkind" {
		rule = "block"
	}
	_, err := s.store.DB().ExecContext(ctx, `DELETE FROM community_kind_rules WHERE kind=?`, kind)
	if err == nil && rule != "" {
		_, err = s.store.DB().ExecContext(ctx, `INSERT INTO community_kind_rules(kind,rule) VALUES(?,?)`, kind, rule)
	}
	return true, err
}
func (s *Service) listKinds(ctx context.Context, rule string) (any, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT kind FROM community_kind_rules WHERE rule=? ORDER BY kind`, strings.TrimSuffix(rule, "kinds"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var k int
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
func (s *Service) listBannedEvents(ctx context.Context) (any, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT id,reason,at FROM community_event_bans ORDER BY at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, r string
		var at int64
		if err := rows.Scan(&id, &r, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "reason": r, "at": at})
	}
	return out, rows.Err()
}
func (s *Service) blockIP(ctx context.Context, actor string, p []json.RawMessage, block bool) (any, error) {
	var ip, reason string
	if err := decode(p, 0, &ip); err != nil {
		return nil, err
	}
	if net.ParseIP(ip) == nil {
		return nil, errors.New("invalid: not an IP address")
	}
	if len(p) > 1 {
		_ = decode(p, 1, &reason)
	}
	var err error
	if block {
		_, err = s.store.DB().ExecContext(ctx, `INSERT INTO community_ip_blocks(ip,reason,at) VALUES(?,?,?) ON CONFLICT(ip) DO UPDATE SET reason=excluded.reason,at=excluded.at`, ip, reason, now())
	} else {
		_, err = s.store.DB().ExecContext(ctx, `DELETE FROM community_ip_blocks WHERE ip=?`, ip)
	}
	return map[string]any{"ip": ip, "blocked": block}, err
}
func (s *Service) listIPs(ctx context.Context) (any, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT ip,reason,at FROM community_ip_blocks ORDER BY at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ip, r string
		var at int64
		if err := rows.Scan(&ip, &r, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"ip": ip, "reason": r, "at": at})
	}
	return out, rows.Err()
}

func (s *Service) audit(ctx context.Context) (any, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT id,at,actor,action,target,detail FROM community_audit ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditRow{}
	for rows.Next() {
		var a AuditRow
		if err := rows.Scan(&a.ID, &a.At, &a.Actor, &a.Action, &a.Target, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Record writes one audit row for an action another service carried out.
func (s *Service) Record(ctx context.Context, actor, action, target, detail string) error {
	return s.store.WithTx(ctx, func(tx *sql.Tx) error { return s.recordTx(ctx, tx, actor, action, target, detail) })
}

func (s *Service) recordTx(ctx context.Context, tx *sql.Tx, actor, action, target, detail string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO community_audit(at,actor,action,target,detail) VALUES(?,?,?,?,?)`, now(), actor, action, target, detail)
	return err
}
func (s *Service) reports(ctx context.Context, p []json.RawMessage) (any, error) {
	status := "open"
	if len(p) > 0 {
		_ = decode(p, 0, &status)
	}
	rows, err := s.store.DB().QueryContext(ctx, `SELECT id,reporter,target_event,type,content,at,status,resolved_by,resolved_at,action FROM community_reports WHERE status=? ORDER BY at DESC LIMIT 200`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.ID, &r.Reporter, &r.TargetEvent, &r.Type, &r.Content, &r.At, &r.Status, &r.ResolvedBy, &r.ResolvedAt, &r.Action); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Service) resolveReport(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	var id int64
	var action string
	if err := decode(p, 0, &id); err != nil {
		return nil, err
	}
	if err := decode(p, 1, &action); err != nil {
		return nil, err
	}
	if action != "ban" && action != "delete" && action != "dismiss" {
		return nil, errors.New("invalid: action must be ban, delete or dismiss")
	}
	r, err := s.store.DB().ExecContext(ctx, `UPDATE community_reports SET status='resolved',resolved_by=?,resolved_at=?,action=? WHERE id=? AND status='open'`, actor, now(), action, id)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return nil, errors.New("invalid: no such open report")
	}
	// Blossom keeps a native report index for its endpoint. Keep that index
	// aligned when the shared moderation record is resolved; older stores may
	// not have the table yet.
	_, blobErr := s.store.DB().ExecContext(ctx, `UPDATE blob_reports SET status='resolved',resolved_by=?,resolved_at=?,action=? WHERE target_blob=(SELECT target_event FROM community_reports WHERE id=?) AND status='open'`, actor, now(), action, id)
	if blobErr != nil && !strings.Contains(blobErr.Error(), "no such table") {
		return nil, blobErr
	}
	return map[string]any{"resolved": id, "action": action}, nil
}

func (s *Service) moderationQueue(ctx context.Context) (any, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT target_event,max(type||CASE WHEN content='' THEN '' ELSE ': '||substr(content,1,200) END) FROM community_reports WHERE status='open' AND target_event<>'' GROUP BY target_event ORDER BY max(at) DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{"id": id, "reason": reason})
	}
	return out, rows.Err()
}

func (s *Service) setBlockedWords(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	var words []string
	if err := decode(p, 0, &words); err != nil {
		return nil, errors.New("invalid: give a list of words")
	}
	data, _ := json.Marshal(words)
	_, err := s.store.DB().ExecContext(ctx, `INSERT INTO community_meta(key,value) VALUES('blocked_words',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(data))
	return words, err
}

func (s *Service) purgeKind(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	var kind int
	var days int
	if len(p) > 0 && string(p[0]) != "null" {
		if err := decode(p, 0, &kind); err != nil {
			return nil, err
		}
	}
	if len(p) > 1 {
		if err := decode(p, 1, &days); err != nil {
			return nil, err
		}
	}
	if days < 0 {
		return nil, errors.New("invalid: days out of range")
	}
	cutoff := int64(^uint64(0) >> 1)
	if days > 0 {
		cutoff = now() - int64(days)*86400
	}
	var result sql.Result
	var err error
	if len(p) > 0 && string(p[0]) == "null" {
		result, err = s.store.DB().ExecContext(ctx, `DELETE FROM events WHERE created_at<?`, cutoff)
	} else {
		result, err = s.store.DB().ExecContext(ctx, `DELETE FROM events WHERE kind=? AND created_at<?`, kind, cutoff)
	}
	if err != nil {
		return nil, err
	}
	n, _ := result.RowsAffected()
	return map[string]any{"deleted": n}, nil
}

func (s *Service) setRetention(ctx context.Context, actor string, p []json.RawMessage) (any, error) {
	var kind, days int
	if err := decode(p, 0, &kind); err != nil {
		return nil, err
	}
	if err := decode(p, 1, &days); err != nil {
		return nil, err
	}
	if kind < 0 || days < 1 {
		return nil, errors.New("invalid: retention needs a nonnegative kind and positive days")
	}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO community_retention(kind,days) VALUES(?,?) ON CONFLICT(kind) DO UPDATE SET days=excluded.days`, kind, days); err != nil {
			return err
		}
		return s.recordTx(ctx, tx, actor, "setretention", fmt.Sprint(kind), fmt.Sprint(days))
	})
	return map[string]any{"kind": kind, "days": days}, err
}

func (s *Service) listRetention(ctx context.Context) (any, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT kind,days FROM community_retention ORDER BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]int{}
	for rows.Next() {
		var kind, days int
		if err := rows.Scan(&kind, &days); err != nil {
			return nil, err
		}
		out = append(out, map[string]int{"kind": kind, "days": days})
	}
	return out, rows.Err()
}

// SubmitReport records one report per reporter and hides the target once the
// configured distinct-reporter threshold is reached.
func (s *Service) SubmitReport(ctx context.Context, reporter, target, reportType, content string, threshold int) error {
	return s.SubmitReportTarget(ctx, reporter, target, "event", reportType, content, threshold)
}

// SubmitReportTarget records a NIP-56 report and applies threshold hiding only
// to event targets. Pubkey and blob targets remain moderation records; they
// must not be inserted into hidden_events as if they were event IDs.
func (s *Service) SubmitReportTarget(ctx context.Context, reporter, target, targetType, reportType, content string, threshold int) error {
	if !validPubKey(reporter) || target == "" {
		return errors.New("invalid: report target or reporter")
	}
	if targetType == "" {
		targetType = "event"
	}
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		return s.SubmitReportTx(ctx, tx, reporter, target, targetType, reportType, content, threshold)
	})
}

// SubmitReportTx records a report in a caller-owned transaction. Blob and
// other protocol doors use this to commit their native report row and the
// shared moderation row atomically.
func (s *Service) SubmitReportTx(ctx context.Context, tx *sql.Tx, reporter, target, targetType, reportType, content string, threshold int) error {
	if tx == nil || !validPubKey(reporter) || target == "" {
		return errors.New("invalid: report target or reporter")
	}
	if targetType == "" {
		targetType = "event"
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO community_reports(reporter,target_event,type,content,at) VALUES(?,?,?,?,?)`, reporter, target, reportType, content, now()); err != nil {
		return err
	}
	if threshold <= 0 || targetType != "event" {
		return nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT reporter) FROM community_reports WHERE target_event=? AND status='open'`, target).Scan(&count); err != nil {
		return err
	}
	if count >= threshold {
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO hidden_events(id,reason) VALUES(?,?)`, target, "report")
		return err
	}
	return nil
}
