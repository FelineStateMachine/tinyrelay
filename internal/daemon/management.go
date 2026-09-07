package daemon

// This file contains the second half of the management registry. Keeping the
// adapters here lets HTTP, the plain HTML UI, and future CLI callers share the
// same source-shaped responses while the core Tenant type remains focused on
// protocol admission.

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/domains"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/templates"
)

// executeManagement handles methods that are not owned by community,
// records, configport, or replication. handled is false when the caller
// should continue through the existing registry.
func (t *Tenant) executeManagement(ctx context.Context, actor, method string, params []json.RawMessage) (result any, handled bool, err error) {
	if clientBrowseMethod(method) {
		result, err = t.executeBrowse(ctx, actor, method, params)
		return result, true, err
	}
	if publicManagementMethod(method) {
		return t.executePublicManagement(ctx, actor, method, params)
	}
	if !managementAdapterMethod(method) {
		return nil, false, nil
	}
	role, roleErr := t.community.Role(ctx, actor)
	if roleErr != nil {
		return nil, true, roleErr
	}
	if !managementRoleAllows(method, role) {
		return nil, true, fmt.Errorf("restricted: %s may not %s", roleName(role), method)
	}
	switch method {
	case "listconnectiontemplates":
		return t.connectionTemplates(), true, nil
	case "listconnections":
		return t.connections(ctx)
	case "setconnections":
		return t.setConnections(ctx, params)
	case "changerelayname":
		return t.changeIdentity(ctx, params, "name")
	case "changerelaydescription":
		return t.changeIdentity(ctx, params, "description")
	case "changerelayicon":
		return t.changeIdentity(ctx, params, "icon")
	case "adddomain", "setdomainsite", "checkdomain", "removedomain", "listdomains":
		return t.domainExecute(ctx, actor, method, params)
	case "listblobs":
		return t.listBlobs(ctx, params)
	case "deleteblob":
		return t.deleteBlob(ctx, params)
	case "listsites":
		return t.listSites(ctx)
	case "deleteevent":
		return t.deleteEvent(ctx, params)
	case "listrecentevents":
		return t.recentEvents(ctx, params)
	case "searchevents":
		return t.searchEvents(ctx, params)
	case "storagestats":
		return t.storageStats(ctx)
	case "publishview":
		return t.publishView(ctx, params)
	case "gitstorage":
		return t.gitStorage(ctx, params)
	case "resetrules":
		return t.resetRules(ctx)
	case "getidentity":
		return t.identity(), true, nil
	case "transferowner":
		return t.transferOwner(ctx, actor, params)
	case "forkrelay":
		return t.forkRelay(ctx, actor, params)
	case "deleterelay":
		return t.deleteRelay(ctx, params)
	default:
		return nil, true, fmt.Errorf("unsupported: management method %q", method)
	}
}

func managementAdapterMethod(method string) bool {
	for _, name := range []string{"listconnectiontemplates", "listconnections", "setconnections", "changerelayname", "changerelaydescription", "changerelayicon", "adddomain", "setdomainsite", "checkdomain", "removedomain", "listdomains", "listblobs", "deleteblob", "listsites", "deleteevent", "listrecentevents", "searchevents", "storagestats", "publishview", "gitstorage", "resetrules", "getidentity", "transferowner", "forkrelay", "deleterelay"} {
		if method == name {
			return true
		}
	}
	return false
}

// PublicMethods are backend calls used by the unauthenticated HTML pages. They
// are intentionally separate from the NIP-86 management methods so a public
// page cannot accidentally gain access to privileged search or export APIs.
func PublicMethods() []string {
	return []string{"queryevents", "eventdetail", "feed", "listview", "claiminvite"}
}

func publicManagementMethod(method string) bool {
	for _, name := range PublicMethods() {
		if method == name {
			return true
		}
	}
	return false
}

// ManagementMethods is the source-compatible NIP-86 registry. The hosted
// lease, trial, fuel, and entitlement controls are intentionally absent.
func ManagementMethods() []string {
	return []string{"supportedmethods", "listaudit", "stats", "getpolicy", "setpolicy", "listviews", "banpubkey", "setblockedwords", "allowpubkey", "setmember", "unrulepubkey", "removemember", "listbannedpubkeys", "listallowedpubkeys", "listmembers", "listpeople", "createinvite", "listinvites", "revokeinvite", "listclaims", "listlisthistory", "restorelist", "createclaim", "deleteclaim", "removesubtree", "banevent", "allowevent", "listeventsneedingmoderation", "blockip", "unblockip", "listblockedips", "listreports", "resolvereport", "exportconfig", "importconfig", "deleterelay", "listblobs", "listsites", "deleteblob", "deleteevent", "listrecentevents", "searchevents", "pinevent", "unpinevent", "listpins", "allowkind", "disallowkind", "unrulekind", "storagestats", "gitstorage", "setretention", "listretention", "purgekind", "listallowedkinds", "listblockedkinds", "notifytest", "resetrules", "listpresets", "listconnectiontemplates", "listconnections", "setconnections", "applypreset", "forkrelay", "pullfrom", "pullstatus", "listjobs", "deliverystatus", "addjob", "removejob", "runjob", "backfill", "transferowner", "listdumps", "deletedump", "dumpnow", "backupnow", "listbackups", "deletebackup", "setsuccession", "clearsuccession", "successionstatus", "changerelayname", "changerelaydescription", "changerelayicon", "adddomain", "setdomainsite", "checkdomain", "removedomain", "listdomains"}
}

func managementRoleAllows(method, role string) bool {
	if role == "owner" {
		return true
	}
	if role != "moderator" {
		return false
	}
	for _, name := range []string{"listconnectiontemplates", "listconnections", "listblobs", "listsites", "listrecentevents", "searchevents", "storagestats", "deleteevent"} {
		if method == name {
			return true
		}
	}
	return false
}

func roleName(role string) string {
	if role == "" {
		return "guest"
	}
	return role
}

func (t *Tenant) executePublicManagement(ctx context.Context, actor, method string, params []json.RawMessage) (any, bool, error) {
	if method == "claiminvite" {
		if strings.TrimSpace(actor) == "" {
			return nil, true, errors.New("auth-required: claiminvite")
		}
		code := stringParam(params, 0)
		terms := stringParam(params, 1)
		if code == "" {
			return nil, true, errors.New("invalid: invite code required")
		}
		if _, err := t.community.ClaimWithTerms(ctx, code, actor, terms); err != nil {
			return nil, true, err
		}
		return map[string]any{"claimed": true, "code": code}, true, nil
	}
	if t.Policy().Reads != "open" && strings.TrimSpace(actor) == "" {
		return nil, true, fmt.Errorf("auth-required: %s", method)
	}
	if method == "listview" {
		return t.publicView(ctx, actor, params)
	}
	var filter event.Filter
	var err error
	switch method {
	case "eventdetail":
		id := stringParam(params, 0)
		if id == "" {
			return nil, true, errors.New("invalid: event id required")
		}
		filter = event.Filter{IDs: []string{id}, Tags: map[string][]string{}, Limit: mgmtIntPtr(1)}
	case "queryevents":
		filter, err = publicFilter(params)
	case "feed":
		filter = event.Filter{Kinds: []int{1, 30023}, Tags: map[string][]string{}, Limit: mgmtIntPtr(50)}
	}
	if err != nil {
		return nil, true, err
	}
	keys := []string{}
	if actor != "" {
		keys = []string{actor}
	}
	rows, err := t.Query(ctx, []event.Filter{filter}, relay.Session{PubKeys: keys, RelayURL: t.publicURL})
	if err != nil {
		return nil, true, err
	}
	if method == "eventdetail" {
		if len(rows) == 0 {
			return nil, true, sql.ErrNoRows
		}
		return rows[0], true, nil
	}
	return rows, true, nil
}

func publicFilter(params []json.RawMessage) (event.Filter, error) {
	if len(params) == 0 {
		return event.Filter{Tags: map[string][]string{}, Limit: mgmtIntPtr(50)}, nil
	}
	f, err := event.ParseFilter(params[0])
	if err != nil {
		return event.Filter{}, errors.New("invalid: event filter")
	}
	if f.Limit == nil {
		f.Limit = mgmtIntPtr(50)
	}
	if *f.Limit < 1 {
		return event.Filter{}, errors.New("invalid: filter limit")
	}
	return f, nil
}

func (t *Tenant) publicView(ctx context.Context, actor string, params []json.RawMessage) (any, bool, error) {
	name := stringParam(params, 0)
	if name == "" {
		return nil, true, errors.New("invalid: view name required")
	}
	role, err := t.community.Role(ctx, actor)
	if err != nil {
		return nil, true, err
	}
	owner := role == "owner"
	member := role == "member" || role == "moderator" || owner
	keys := []string{}
	if actor != "" {
		keys = append(keys, actor)
	}
	view, err := t.records.View(ctx, name, policy.Access{Owner: owner, Member: member, PubKeys: keys}, time.Now().Unix())
	if err != nil {
		return nil, true, err
	}
	return view.Event, true, nil
}

func mgmtIntPtr(value int) *int { return &value }

func (t *Tenant) identity() map[string]any {
	p := t.Policy()
	return map[string]any{"owner": p.Owner, "name": p.Name, "description": p.Description, "icon": p.Icon, "relayURL": t.publicURL, "identity": t.records.PublicKey()}
}

func (t *Tenant) transferOwner(ctx context.Context, actor string, params []json.RawMessage) (any, bool, error) {
	next := stringParam(params, 0)
	if next == "" || len(next) != 64 {
		return nil, true, errors.New("invalid: owner public key required")
	}
	if _, err := hex.DecodeString(next); err != nil {
		return nil, true, errors.New("invalid: owner public key required")
	}
	current := t.Policy().Owner
	if actor != current {
		return nil, true, errors.New("restricted: owner required")
	}
	nextPolicy, err := policy.Patch(t.Policy(), map[string]json.RawMessage{"owner": json.RawMessage(strconv.Quote(next))})
	if err != nil {
		return nil, true, err
	}
	if err := t.applyPolicy(ctx, nextPolicy); err != nil {
		return nil, true, err
	}
	return map[string]any{"owner": next, "previousOwner": current}, true, nil
}

func (t *Tenant) deleteRelay(ctx context.Context, params []json.RawMessage) (any, bool, error) {
	name := stringParam(params, 0)
	if name == "" || !strings.EqualFold(name, t.meta.Name) {
		return nil, true, errors.New("invalid: relay name confirmation required")
	}
	if err := t.app.deleteTenant(ctx, t.meta.ID); err != nil {
		return nil, true, err
	}
	return map[string]any{"deleted": true, "name": t.meta.Name}, true, nil
}

func (t *Tenant) forkRelay(ctx context.Context, actor string, params []json.RawMessage) (any, bool, error) {
	if len(params) != 1 {
		return nil, true, errors.New("invalid: fork options required")
	}
	var options struct {
		Name   string        `json:"name"`
		Owner  string        `json:"owner"`
		Kinds  []int         `json:"kinds"`
		People bool          `json:"people"`
		Filter *event.Filter `json:"filter"`
	}
	if err := json.Unmarshal(params[0], &options); err != nil {
		return nil, true, errors.New("invalid: fork options")
	}
	if strings.TrimSpace(options.Name) == "" {
		options.Name = fmt.Sprintf("fork-%d", time.Now().UnixNano())
	}
	if options.Owner == "" {
		options.Owner = actor
	}
	if len(options.Owner) != 64 {
		return nil, true, errors.New("invalid: fork owner")
	}
	meta, err := t.app.Create(ctx, catalog.CreateOptions{Name: options.Name, Owner: options.Owner, Template: t.meta.Template})
	if err != nil {
		return nil, true, err
	}
	newURL := t.app.forkRelayURL(t, meta.Name)
	target, err := t.app.tenant(ctx, meta, newURL)
	if err != nil {
		return nil, true, err
	}
	filter := event.Filter{Tags: map[string][]string{}}
	if options.Filter != nil {
		filter = *options.Filter
		if filter.Tags == nil {
			filter.Tags = map[string][]string{}
		}
	}
	if len(options.Kinds) > 0 && len(filter.Kinds) == 0 {
		filter.Kinds = options.Kinds
	}
	rows, err := t.store.Query(ctx, filter, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 0})
	if err != nil {
		return nil, true, err
	}
	for _, copied := range rows.Events {
		if _, err := target.store.Save(ctx, copied, storage.SaveOptions{Now: time.Now().Unix(), SearchMode: target.Policy().Features.Search}); err != nil {
			return nil, true, fmt.Errorf("fork event %s: %w", copied.ID, err)
		}
	}
	if err := copyForkConfig(ctx, t.store, target.store); err != nil {
		return nil, true, err
	}
	if options.People {
		if err := copyForkPeople(ctx, t.store, target.store, options.Owner); err != nil {
			return nil, true, err
		}
	}
	return map[string]any{"id": meta.ID, "name": meta.Name, "owner": meta.Owner, "relayURL": newURL, "events": len(rows.Events), "people": options.People}, true, nil
}

func (a *App) forkRelayURL(source *Tenant, name string) string {
	base := strings.TrimSuffix(a.cfg.PublicURL, "/")
	if base != "" {
		return base + "/r/" + name
	}
	u, err := url.Parse(source.publicURL)
	if err != nil {
		return strings.TrimSuffix(source.publicURL, "/") + "/r/" + name
	}
	prefix := "/r/" + source.meta.Name
	if strings.HasSuffix(u.Path, prefix) {
		u.Path = strings.TrimSuffix(u.Path, prefix)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/r/" + name
	return strings.TrimSuffix(u.String(), "/")
}

func copyForkConfig(ctx context.Context, source, target *storage.Store) error {
	return target.WithTx(ctx, func(tx *sql.Tx) error {
		for _, table := range []struct {
			name   string
			insert string
			query  string
		}{
			{"config_connections", "INSERT OR REPLACE INTO config_connections(position,value) VALUES(?,?)", "SELECT position,value FROM config_connections ORDER BY position"},
			{"community_kind_rules", "INSERT OR REPLACE INTO community_kind_rules(kind,rule) VALUES(?,?)", "SELECT kind,rule FROM community_kind_rules"},
			{"community_retention", "INSERT OR REPLACE INTO community_retention(kind,days) VALUES(?,?)", "SELECT kind,days FROM community_retention"},
		} {
			rows, err := source.DB().QueryContext(ctx, table.query)
			if err != nil {
				return fmt.Errorf("fork %s: %w", table.name, err)
			}
			for rows.Next() {
				var first, second any
				if err := rows.Scan(&first, &second); err != nil {
					_ = rows.Close()
					return err
				}
				if _, err := tx.ExecContext(ctx, table.insert, first, second); err != nil {
					_ = rows.Close()
					return err
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			_ = rows.Close()
		}
		return nil
	})
}

func copyForkPeople(ctx context.Context, source, target *storage.Store, owner string) error {
	rows, err := source.DB().QueryContext(ctx, `SELECT pubkey,name,note,role,invited_by,via,created_at,keep_days FROM community_members WHERE role<>'owner'`)
	if err != nil {
		return fmt.Errorf("fork members: %w", err)
	}
	defer rows.Close()
	return target.WithTx(ctx, func(tx *sql.Tx) error {
		for rows.Next() {
			var pubkey, name, note, role, invitedBy, via string
			var createdAt, keepDays int64
			if err := rows.Scan(&pubkey, &name, &note, &role, &invitedBy, &via, &createdAt, &keepDays); err != nil {
				return err
			}
			if pubkey == owner {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO community_members(pubkey,name,note,role,invited_by,via,created_at,keep_days) VALUES(?,?,?,?,?,?,?,?)`, pubkey, name, note, role, invitedBy, via, createdAt, keepDays); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// AddressFilterForPath turns the plain HTML address routes into storage
// filters. It is also used by NIP-AD discovery handlers to keep route and
// discovery semantics aligned.
func AddressFilterForPath(rawPath, author, identity string) (map[string]any, bool) {
	p := strings.TrimPrefix(path.Clean("/"+rawPath), "/")
	parts := strings.Split(p, "/")
	if len(parts) != 2 || parts[1] == "" {
		return nil, false
	}
	value, err := url.PathUnescape(parts[1])
	if err != nil || strings.ContainsAny(value, "/\\") {
		return nil, false
	}
	switch parts[0] {
	case "e":
		if len(value) != 64 {
			return nil, false
		}
		if _, err := hex.DecodeString(value); err != nil {
			return nil, false
		}
		return map[string]any{"ids": []string{value}, "kinds": []int{1, 30023}, "limit": 1}, true
	case "a":
		if author == "" {
			return nil, false
		}
		return map[string]any{"authors": []string{author}, "kinds": []int{30023}, "#d": []string{value}, "limit": 1}, true
	case "view":
		if identity == "" {
			return nil, false
		}
		return map[string]any{"authors": []string{identity}, "kinds": []int{30078}, "#d": []string{"bind.ws/view/" + value}, "limit": 1}, true
	default:
		return nil, false
	}
}

func (t *Tenant) connectionTemplates() []map[string]any {
	result := make([]map[string]any, 0)
	p := t.Policy()
	for _, c := range templates.Connections() {
		available := c.Feature == "" || featureEnabled(p, c.Feature)
		result = append(result, map[string]any{"name": c.Name, "title": c.Title, "about": c.About, "app": c.App, "where": c.Where, "icon": c.Icon, "feature": c.Feature, "visibility": c.Visibility, "qr": c.QR, "inputs": c.Inputs, "links": c.Links, "available": available})
	}
	return result
}

func featureEnabled(p policy.Policy, name string) bool {
	switch name {
	case "files":
		return p.Features.Files
	case "sites":
		return p.Features.Sites.Enabled
	case "grasp":
		return p.Features.Grasp
	case "marmot":
		return p.Features.Marmot
	case "search":
		return p.Features.Search != "off"
	default:
		return false
	}
}

func (t *Tenant) connections(ctx context.Context) (any, bool, error) {
	rows, err := t.store.DB().QueryContext(ctx, `SELECT value FROM config_connections ORDER BY position`)
	if err != nil {
		return nil, true, fmt.Errorf("list connections: %w", err)
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, true, err
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return nil, true, fmt.Errorf("decode connection: %w", err)
		}
		result = append(result, value)
	}
	return result, true, rows.Err()
}

func (t *Tenant) setConnections(ctx context.Context, params []json.RawMessage) (any, bool, error) {
	if len(params) != 1 {
		return nil, true, errors.New("invalid: setconnections expects one list")
	}
	var values []map[string]any
	if err := json.Unmarshal(params[0], &values); err != nil {
		return nil, true, errors.New("invalid: connections must be a list")
	}
	if err := t.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM config_connections`); err != nil {
			return err
		}
		for i, value := range values {
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO config_connections(position,value) VALUES(?,?)`, i, raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, true, fmt.Errorf("save connections: %w", err)
	}
	return values, true, nil
}

func (t *Tenant) changeIdentity(ctx context.Context, params []json.RawMessage, key string) (any, bool, error) {
	if len(params) != 1 {
		return nil, true, errors.New("invalid: identity value required")
	}
	var value string
	if err := json.Unmarshal(params[0], &value); err != nil {
		return nil, true, errors.New("invalid: identity value must be a string")
	}
	limit := 2000
	if key == "name" {
		limit = 200
	}
	if len(value) > limit {
		value = value[:limit]
	}
	_, err := t.setPolicy(ctx, map[string]json.RawMessage{key: json.RawMessage(strconv.Quote(value))})
	return true, true, err
}

func (t *Tenant) domainExecute(ctx context.Context, actor, method string, params []json.RawMessage) (any, bool, error) {
	s, err := domains.New(domains.Config{Catalog: t.app.catalog, TenantID: t.meta.ID, BaseHost: hostName(t.publicURL), RelayURL: t.publicURL, Owner: func(key string) bool { return key == t.Policy().Owner }})
	if err != nil {
		return nil, true, err
	}
	value, err := s.Execute(ctx, actor, method, params)
	return value, true, err
}

func (t *Tenant) listBlobs(ctx context.Context, params []json.RawMessage) (any, bool, error) {
	limit := 100
	if len(params) > 0 {
		_ = json.Unmarshal(params[0], &limit)
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := t.store.DB().QueryContext(ctx, `SELECT sha256,size,type,uploader,uploaded FROM blobs ORDER BY uploaded DESC LIMIT ?`, limit)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var sha, typ, uploader string
		var size, uploaded int64
		if err := rows.Scan(&sha, &size, &typ, &uploader, &uploaded); err != nil {
			return nil, true, err
		}
		result = append(result, map[string]any{"sha256": sha, "size": size, "type": typ, "uploaded": uploaded, "uploader": uploader, "url": "/" + sha})
	}
	return result, true, rows.Err()
}

func (t *Tenant) deleteBlob(ctx context.Context, params []json.RawMessage) (any, bool, error) {
	if t.blobs == nil || len(params) != 1 {
		return nil, true, errors.New("invalid: sha256 required")
	}
	var sha string
	if err := json.Unmarshal(params[0], &sha); err != nil {
		return nil, true, errors.New("invalid: sha256 required")
	}
	return true, true, t.blobs.Delete(ctx, sha)
}

func (t *Tenant) listSites(ctx context.Context) (any, bool, error) {
	rows, err := t.store.Query(ctx, event.Filter{Kinds: []int{sites.KindSite, sites.KindNamedSite, sites.KindSiteSnapshot}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 0})
	if err != nil {
		return nil, true, err
	}
	result := make([]map[string]any, 0, len(rows.Events))
	for _, e := range rows.Events {
		label := sites.SiteLabel(e)
		if label == "" {
			continue
		}
		result = append(result, map[string]any{"id": e.ID, "kind": e.Kind, "author": e.PubKey, "label": label, "paths": len(sites.SitePaths(e))})
	}
	return result, true, nil
}

func (t *Tenant) deleteEvent(ctx context.Context, params []json.RawMessage) (any, bool, error) {
	if len(params) != 1 {
		return nil, true, errors.New("invalid: event id required")
	}
	var id string
	if err := json.Unmarshal(params[0], &id); err != nil {
		return nil, true, err
	}
	deleted, err := t.store.DeleteEvent(ctx, id)
	return deleted, true, err
}

func (t *Tenant) recentEvents(ctx context.Context, params []json.RawMessage) (any, bool, error) {
	limit := intParam(params, 0, 50, 500)
	rows, err := t.store.Query(ctx, event.Filter{Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: limit})
	if err != nil {
		return nil, true, err
	}
	return rows.Events, true, nil
}

func (t *Tenant) searchEvents(ctx context.Context, params []json.RawMessage) (any, bool, error) {
	query := stringParam(params, 0)
	limit := intParam(params, 1, 50, 200)
	rows, err := t.store.Query(ctx, event.Filter{Search: query, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: limit})
	if err != nil {
		return nil, true, err
	}
	return rows.Events, true, nil
}

func (t *Tenant) storageStats(ctx context.Context) (any, bool, error) {
	stats, err := t.store.Stats(ctx)
	return stats, true, err
}

func (t *Tenant) publishView(ctx context.Context, params []json.RawMessage) (any, bool, error) {
	name := stringParam(params, 0)
	if name == "" {
		return nil, true, errors.New("invalid: view name required")
	}
	if err := t.records.MarkView(ctx, name, time.Now().Unix()); err != nil {
		return nil, true, err
	}
	view, err := t.records.View(ctx, name, policy.Access{Owner: true}, time.Now().Unix())
	return view, true, err
}

func (t *Tenant) gitStorage(ctx context.Context, params []json.RawMessage) (any, bool, error) {
	if t.git == nil {
		return nil, true, errors.New("unsupported: git storage is disabled")
	}
	if len(params) != 2 {
		return nil, true, errors.New("invalid: repository owner and identifier required")
	}
	owner, identifier := stringParam(params, 0), stringParam(params, 1)
	if len(owner) != 64 {
		return nil, true, errors.New("invalid: repository owner")
	}
	if _, err := hex.DecodeString(owner); err != nil || identifier == "" || len(identifier) > 256 {
		return nil, true, errors.New("invalid: repository owner and identifier")
	}
	result, err := t.git.StorageInventory(ctx, owner, identifier)
	return result, true, err
}

func (t *Tenant) resetRules(ctx context.Context) (any, bool, error) {
	if err := t.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM community_kind_rules"); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, true, err
	}
	if _, err := t.setPolicy(ctx, map[string]json.RawMessage{
		"blockedWords": json.RawMessage(`[]`),
		"openKinds":    json.RawMessage(`[]`),
	}); err != nil {
		return nil, true, err
	}
	return map[string]any{"reset": true}, true, nil
}

func intParam(params []json.RawMessage, index, fallback, max int) int {
	if len(params) <= index {
		return fallback
	}
	var value int
	if json.Unmarshal(params[index], &value) != nil {
		return fallback
	}
	if value < 1 {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}
func stringParam(params []json.RawMessage, index int) string {
	if len(params) <= index {
		return ""
	}
	var value string
	_ = json.Unmarshal(params[index], &value)
	return strings.TrimSpace(value)
}
