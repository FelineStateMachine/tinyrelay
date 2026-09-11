package daemon

// Contextual file browsing. The legacy (empty View) response remains the
// Blossom inventory used by MCP clients; the views below are for the web UI.
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func (t *Tenant) browseFiles(ctx context.Context, actor string, q clientBrowseRequest) (any, error) {
	if q.View == "" {
		return t.browseFilesLegacy(ctx, actor, q)
	}
	if !t.Policy().Features.Files {
		return nil, errors.New("not found: files are disabled")
	}
	if actor == "" {
		return nil, errors.New("auth-required: sign in to browse your files")
	}
	role, err := t.community.Role(ctx, actor)
	if err != nil {
		return nil, err
	}
	canManage := role == "owner" || role == "moderator"
	var result any
	switch q.View {
	case "library":
		result, err = t.browseLibrary(ctx, actor, q)
	case "sites":
		result, err = t.browseSiteFiles(ctx, actor, q)
	case "rooms":
		result, err = t.browseRoomFiles(ctx, actor, q)
	case "storage":
		if !canManage {
			return nil, errors.New("restricted: storage inventory requires owner or moderator")
		}
		result, err = t.browseStorage(ctx, actor, q)
	default:
		return nil, errors.New("invalid: file view")
	}
	if err != nil {
		return nil, err
	}
	if row, ok := result.(map[string]any); ok {
		row["can_manage"] = canManage
	}
	return result, nil
}

func (t *Tenant) browseFilesLegacy(ctx context.Context, actor string, q clientBrowseRequest) (any, error) {
	if actor == "" {
		return nil, errors.New("auth-required: sign in to browse your files")
	}
	role, err := t.community.Role(ctx, actor)
	if err != nil {
		return nil, err
	}
	var entries []blob.Blob
	var next string
	if role == "owner" || role == "moderator" {
		entries, next, err = t.browseAllFiles(ctx, q)
	} else if q.Query != "" {
		cursor := q.Cursor
		for len(entries) < q.Limit {
			var page []blob.Blob
			page, next, err = t.blobs.ListPage(ctx, actor, q.Limit-len(entries), cursor)
			if err != nil {
				break
			}
			for _, e := range page {
				if blobMatchesQuery(e, q.Query) {
					entries = append(entries, e)
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	} else {
		entries, next, err = t.blobs.ListPage(ctx, actor, q.Limit, q.Cursor)
	}
	if err != nil {
		return nil, err
	}
	items := []map[string]any{}
	for _, e := range entries {
		if q.Query == "" || blobMatchesQuery(e, q.Query) {
			items = append(items, t.browseBlobMetadata(e))
		}
	}
	return map[string]any{"items": items, "next_cursor": next}, nil
}

func (t *Tenant) browseAllFiles(ctx context.Context, q clientBrowseRequest) ([]blob.Blob, string, error) {
	query := "SELECT sha256,size,type,uploader,uploaded FROM blobs WHERE sha256>?"
	args := []any{q.Cursor}
	needle := strings.ToLower(strings.TrimSpace(q.Query))
	if needle != "" {
		query += " AND (instr(lower(sha256),?)>0 OR instr(lower(type),?)>0 OR instr(lower(uploader),?)>0)"
		args = append(args, needle, needle, needle)
	}
	query += " ORDER BY sha256 LIMIT ?"
	args = append(args, q.Limit+1)
	rows, err := t.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []blob.Blob{}
	for rows.Next() {
		var e blob.Blob
		if err := rows.Scan(&e.SHA256, &e.Size, &e.Type, &e.Uploader, &e.Uploaded); err != nil {
			return nil, "", err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > q.Limit {
		next = out[q.Limit-1].SHA256
		out = out[:q.Limit]
	}
	return out, next, nil
}
func blobMatchesQuery(e blob.Blob, q string) bool {
	n := strings.ToLower(strings.TrimSpace(q))
	return n == "" || strings.Contains(strings.ToLower(e.SHA256), n) || strings.Contains(strings.ToLower(e.Type), n) || strings.Contains(strings.ToLower(e.Uploader), n)
}
func (t *Tenant) browseBlobMetadata(e blob.Blob) map[string]any {
	portable := blob.BlossomURI{Hash: e.SHA256, Extension: "bin", Author: e.Uploader, Size: e.Size}
	if origin, err := url.Parse(t.publicURL); err == nil && strings.Trim(origin.Path, "/") == "" {
		portable.Servers = []string{origin.Scheme + "://" + origin.Host}
	}
	return map[string]any{"sha256": e.SHA256, "size": e.Size, "type": e.Type, "uploader": e.Uploader, "uploaded": e.Uploaded, "url": strings.TrimRight(t.publicURL, "/") + "/" + e.SHA256, "download_url": strings.TrimRight(t.publicURL, "/") + "/files/raw?hash=" + url.QueryEscape(e.SHA256), "blossom_uri": portable.String()}
}

func (t *Tenant) claimRows(ctx context.Context, actor string, all bool) ([]map[string]any, error) {
	q := `SELECT b.sha256,b.size,b.type,c.uploader,b.uploaded,COALESCE(m.name,''),COALESCE(m.path,''),COALESCE(m.purpose,''),COALESCE(m.logical_size,0),COALESCE(m.logical_type,''),b.access FROM blobs b JOIN blob_claims c ON c.sha256=b.sha256 LEFT JOIN blob_claim_metadata m ON m.sha256=b.sha256 AND m.uploader=c.uploader`
	args := []any{}
	if !all {
		includeMembers := false
		if role, roleErr := t.community.Role(ctx, actor); roleErr == nil && t.memberBlobReader(ctx, actor, role) {
			includeMembers = true
		}
		if includeMembers {
			q += " WHERE c.uploader=? OR b.access=?"
			args = append(args, actor, blob.AccessMembers)
		} else {
			q += " WHERE c.uploader=?"
			args = append(args, actor)
		}
	}
	q += " ORDER BY b.uploaded DESC,b.sha256 DESC"
	rows, err := t.store.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var e blob.Blob
		var name, p, purpose, logicalType, access string
		var logicalSize int64
		if err := rows.Scan(&e.SHA256, &e.Size, &e.Type, &e.Uploader, &e.Uploaded, &name, &p, &purpose, &logicalSize, &logicalType, &access); err != nil {
			return nil, err
		}
		m := t.browseBlobMetadata(e)
		m["access"] = access
		m["name"] = name
		m["path"] = p
		m["purpose"] = purpose
		if purpose == "file" && logicalType != "" {
			m["logical_size"] = logicalSize
		}
		if logicalType != "" {
			m["logical_type"] = logicalType
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (t *Tenant) browseLibrary(ctx context.Context, actor string, q clientBrowseRequest) (any, error) {
	rows, err := t.claimRows(ctx, actor, false)
	if err != nil {
		return nil, err
	}
	excluded := map[string]bool{}
	paths, rooms, err := t.contextHashes(ctx, actor)
	if err != nil {
		return nil, err
	}
	for h := range paths {
		excluded[h] = true
	}
	for h := range rooms {
		excluded[h] = true
	}
	rows = libraryRows(rows, excluded)
	return t.folderItems(rows, excluded, q, "library", false), nil
}

// libraryRows keeps implementation objects out of the user's file library.
// New encrypted uploads mark their chunk claims with purpose=chunk. Older
// unnamed octet streams cannot be identified safely from size alone, so they
// remain visible in one unorganized folder instead of being deleted or
// silently hidden.
func libraryRows(rows []map[string]any, excluded map[string]bool) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row["purpose"] == "chunk" {
			continue
		}
		if row["purpose"] == "file" {
			if size, ok := row["logical_size"]; ok {
				row["size"] = size
			}
		}
		if excluded[row["sha256"].(string)] && row["name"] == "" {
			continue
		}
		if row["name"] == "" && row["path"] == "" && row["type"] == "application/octet-stream" {
			hash := row["sha256"].(string)
			short := hash
			if len(short) > 12 {
				short = short[:12]
			}
			row["path"] = "Unorganized uploads/" + short
			row["name"] = short
		}
		out = append(out, row)
	}
	return out
}
func (t *Tenant) folderItems(rows []map[string]any, excluded map[string]bool, q clientBrowseRequest, view string, includeExcluded bool) map[string]any {
	prefix := strings.Trim(q.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	folders := map[string]bool{}
	files := []map[string]any{}
	needle := strings.ToLower(strings.TrimSpace(q.Query))
	for _, m := range rows {
		if !includeExcluded && excluded[m["sha256"].(string)] && m["name"] == "" {
			continue
		}
		p := m["path"].(string)
		name := m["name"].(string)
		if p == "" {
			if name != "" {
				p = name
			} else {
				short := m["sha256"].(string)
				if len(short) > 12 {
					short = short[:12]
				}
				p = "(" + strings.ReplaceAll(m["type"].(string), "/", "-") + ") " + short
			}
		}
		if needle != "" && !strings.Contains(strings.ToLower(p+" "+m["type"].(string)+" "+m["sha256"].(string)), needle) {
			continue
		}
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			folders[rest[:i]] = true
			continue
		}
		if p != "" {
			m["path"] = p
			m["name"] = path.Base(p)
		}
		m["kind"] = "file"
		m["open"] = "/file?hash=" + url.QueryEscape(m["sha256"].(string))
		if contextName, ok := m["context"].(string); ok {
			m["context_url"] = "/files?view=" + contextName + "s&path=" + url.QueryEscape(path.Dir(p))
		}
		files = append(files, m)
	}
	items := []map[string]any{}
	for n := range folders {
		items = append(items, map[string]any{"kind": "folder", "name": n, "path": strings.Trim(prefix+n, "/"), "open": "/files?view=" + url.QueryEscape(view) + "&path=" + url.QueryEscape(strings.Trim(prefix+n, "/"))})
	}
	items = append(items, files...)
	sort.Slice(items, func(i, j int) bool { return fileItemKey(items[i]) < fileItemKey(items[j]) })
	if q.Cursor != "" {
		cursor := q.Cursor
		if decoded, err := base64.RawURLEncoding.DecodeString(cursor); err == nil {
			cursor = string(decoded)
		}
		position := sort.Search(len(items), func(i int) bool { return fileItemKey(items[i]) > cursor })
		items = items[position:]
	}
	next := ""
	if len(items) > q.Limit {
		next = base64.RawURLEncoding.EncodeToString([]byte(fileItemKey(items[q.Limit-1])))
		items = items[:q.Limit]
	}
	return map[string]any{"items": items, "next_cursor": next, "breadcrumbs": breadcrumbs(view, prefix), "view": view, "path": strings.Trim(prefix, "/"), "can_manage": false}
}
func fileItemKey(item map[string]any) string {
	if item["kind"] == "folder" {
		return "0\x00" + item["path"].(string)
	}
	return "1\x00" + item["path"].(string) + "\x00" + item["sha256"].(string)
}
func breadcrumbs(view, p string) []map[string]string {
	label := map[string]string{"library": "My files", "sites": "Sites", "rooms": "Rooms", "storage": "Storage"}[view]
	out := []map[string]string{{"name": label, "open": "/files?view=" + view}}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	cur := ""
	for _, v := range parts {
		if v == "" {
			continue
		}
		if cur != "" {
			cur += "/"
		}
		cur += v
		out = append(out, map[string]string{"name": v, "open": "/files?view=" + view + "&path=" + url.QueryEscape(cur)})
	}
	return out
}

func (t *Tenant) contextHashes(ctx context.Context, actor string) (map[string][]string, map[string][]string, error) {
	sitesH := map[string][]string{}
	roomsH := map[string][]string{}
	var before *storage.EventCursor
	for {
		r, err := t.store.Query(ctx, event.Filter{Kinds: []int{sites.KindSite, sites.KindNamedSite, sites.KindSiteSnapshot}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{PubKeys: browseSession(t, actor).PubKeys}, Limit: 100, Before: before})
		if err != nil {
			return nil, nil, err
		}
		for _, e := range r.Events {
			label := sites.SiteLabel(e)
			for _, p := range sites.SitePaths(e) {
				if len(p) == 3 {
					sitesH[p[2]] = append(sitesH[p[2]], label+"/"+strings.TrimPrefix(p[1], "/"))
				}
			}
		}
		if !r.More || len(r.Events) == 0 {
			break
		}
		last := r.Events[len(r.Events)-1]
		before = &storage.EventCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	rs, err := t.community.Rooms(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, rm := range rs {
		if _, _, e := t.roomFor(ctx, actor, rm.ID); e != nil {
			continue
		}
		rows, e := t.store.DB().QueryContext(ctx, "SELECT sha256,filename FROM room_attachments WHERE room_id=? AND room_event_id=?", rm.ID, rm.EventID)
		if e != nil {
			return nil, nil, e
		}
		for rows.Next() {
			var h, filename string
			if e := rows.Scan(&h, &filename); e != nil {
				rows.Close()
				return nil, nil, e
			}
			roomsH[h] = append(roomsH[h], rm.ID+"/"+roomAttachmentFilename(filename))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()
	}
	return sitesH, roomsH, nil
}

func (t *Tenant) browseStorage(ctx context.Context, actor string, q clientBrowseRequest) (any, error) {
	query := `SELECT sha256,size,type,uploader,uploaded FROM blobs WHERE 1=1`
	args := []any{}
	if q.Cursor != "" {
		var cursor struct {
			Uploaded int64
			Hash     string
		}
		decoded, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(decoded, &cursor) != nil || len(cursor.Hash) != 64 {
			return nil, errors.New("invalid: storage cursor")
		}
		query += " AND (uploaded<? OR (uploaded=? AND sha256<?))"
		args = append(args, cursor.Uploaded, cursor.Uploaded, cursor.Hash)
	}
	if needle := strings.ToLower(strings.TrimSpace(q.Query)); needle != "" {
		query += " AND (instr(lower(sha256),?)>0 OR instr(lower(type),?)>0 OR instr(lower(uploader),?)>0)"
		args = append(args, needle, needle, needle)
	}
	query += " ORDER BY uploaded DESC,sha256 DESC LIMIT ?"
	args = append(args, q.Limit+1)
	rows, err := t.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var entry blob.Blob
		if err := rows.Scan(&entry.SHA256, &entry.Size, &entry.Type, &entry.Uploader, &entry.Uploaded); err != nil {
			return nil, err
		}
		item := t.browseBlobMetadata(entry)
		item["kind"], item["name"], item["path"] = "file", entry.SHA256[:12], ""
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	next := ""
	if len(items) > q.Limit {
		last := items[q.Limit-1]
		encoded, _ := json.Marshal(struct {
			Uploaded int64
			Hash     string
		}{last["uploaded"].(int64), last["sha256"].(string)})
		next = base64.RawURLEncoding.EncodeToString(encoded)
		items = items[:q.Limit]
	}
	return map[string]any{"items": items, "view": "storage", "path": "", "breadcrumbs": breadcrumbs("storage", ""), "next_cursor": next}, nil
}
func (t *Tenant) browseRoomFiles(ctx context.Context, actor string, q clientBrowseRequest) (any, error) {
	_, rooms, err := t.contextHashes(ctx, actor)
	if err != nil {
		return nil, err
	}
	rows, err := t.contextFileRows(ctx, rooms, "room")
	if err != nil {
		return nil, err
	}
	result := t.folderItems(rows, map[string]bool{}, q, "rooms", true)
	allRooms, err := t.community.Rooms(ctx)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for _, room := range allRooms {
		labels[room.ID] = room.Name
	}
	for _, item := range result["items"].([]map[string]any) {
		root := strings.SplitN(item["path"].(string), "/", 2)[0]
		if q.Path == "" {
			item["kind"] = "room"
			if labels[root] != "" {
				item["name"] = labels[root]
			}
		}
		item["context"], item["context_url"] = labels[root], "/rooms/"+url.PathEscape(root)
	}
	crumbs := result["breadcrumbs"].([]map[string]string)
	if len(crumbs) > 1 && labels[crumbs[1]["name"]] != "" {
		crumbs[1]["name"] = labels[crumbs[1]["name"]]
	}
	return result, nil
}
func (t *Tenant) browseSiteFiles(ctx context.Context, actor string, q clientBrowseRequest) (any, error) {
	sitesH, _, err := t.contextHashes(ctx, actor)
	if err != nil {
		return nil, err
	}
	rows, err := t.contextFileRows(ctx, sitesH, "site")
	if err != nil {
		return nil, err
	}
	result := t.folderItems(rows, map[string]bool{}, q, "sites", true)
	if q.Path == "" {
		for _, item := range result["items"].([]map[string]any) {
			item["kind"] = "site"
		}
	}
	return result, nil
}

// Read only the referenced blobs. Claim labels belong to uploaders and must
// not become names for files viewed through someone else's site or room.
func (t *Tenant) contextFileRows(ctx context.Context, refs map[string][]string, contextName string) ([]map[string]any, error) {
	hashes := make([]string, 0, len(refs))
	for hash := range refs {
		hashes = append(hashes, hash)
	}
	items := []map[string]any{}
	for start := 0; start < len(hashes); start += 250 {
		encoded, _ := json.Marshal(hashes[start:min(start+250, len(hashes))])
		rows, err := t.store.DB().QueryContext(ctx, `SELECT sha256,size,type,uploader,uploaded FROM blobs WHERE sha256 IN (SELECT value FROM json_each(?))`, string(encoded))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var entry blob.Blob
			if err := rows.Scan(&entry.SHA256, &entry.Size, &entry.Type, &entry.Uploader, &entry.Uploaded); err != nil {
				rows.Close()
				return nil, err
			}
			item := t.browseBlobMetadata(entry)
			delete(item, "uploader")
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return contextualRows(items, refs, contextName), nil
}

func contextualRows(rows []map[string]any, refs map[string][]string, contextName string) []map[string]any {
	out := make([]map[string]any, 0, len(refs))
	seen := map[string]bool{}
	for _, row := range rows {
		hash, _ := row["sha256"].(string)
		paths, ok := refs[hash]
		if !ok {
			continue
		}
		for _, ref := range paths {
			key := contextName + "\x00" + ref + "\x00" + hash
			if seen[key] {
				continue
			}
			seen[key] = true
			copy := make(map[string]any, len(row)+3)
			for k, v := range row {
				copy[k] = v
			}
			copy["path"] = ref
			copy["name"] = path.Base(ref)
			copy["context"] = contextName
			out = append(out, copy)
		}
	}
	return out
}
