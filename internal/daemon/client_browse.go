package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type clientBrowseRequest struct {
	gitrelay.BrowseRequest
	Cursor string `json:"cursor"`
	Query  string `json:"q"`
	Hash   string `json:"hash"`
	Event  string `json:"event"`
	ID     string `json:"id"`
	State  string `json:"state"`
	Label  string `json:"label"`
	Agent  string `json:"agent"`
}

func clientBrowseMethod(method string) bool {
	return wikiBrowseMethod(method) || approvalBrowseMethod(method) || jobBrowseMethod(method) || containsString([]string{"browserepos", "browserepo", "browsefiles", "browsefile", "browsestatus", "browseissues", "browsepulls", "browseissue", "browsepull", "browseagent"}, method)
}

func (t *Tenant) browseRead(ctx context.Context, actor string) error {
	keys := []string{}
	if actor != "" {
		keys = append(keys, actor)
	}
	_, err := t.gate.Read(ctx, []event.Filter{{}}, relay.Session{PubKeys: keys, RelayURL: t.RelayURL()})
	return err
}

func (t *Tenant) executeBrowse(ctx context.Context, actor, method string, params []json.RawMessage) (result any, err error) {
	ctx, finish := t.app.telemetry.Start(ctx, method)
	defer func() {
		if err != nil {
			finish("error")
		} else {
			finish("success")
		}
	}()
	if err := t.browseRead(ctx, actor); err != nil {
		return nil, err
	}
	if wikiBrowseMethod(method) {
		return t.executeWiki(ctx, actor, method, params)
	}
	if approvalBrowseMethod(method) {
		return t.executeApprovals(ctx, actor, method, params)
	}
	if jobBrowseMethod(method) {
		return t.executeJobs(ctx, actor, method, params)
	}
	q := clientBrowseRequest{}
	if len(params) > 0 {
		if err := json.Unmarshal(params[0], &q); err != nil {
			return nil, fmt.Errorf("invalid: browse parameters: %w", err)
		}
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 50
	}
	switch method {
	case "browserepos":
		return t.browseRepos(ctx, actor, q)
	case "browserepo":
		return t.browseRepo(ctx, actor, q)
	case "browsefiles":
		return t.browseFiles(ctx, actor, q)
	case "browsefile":
		return t.browseFile(ctx, actor, q.Hash)
	case "browsestatus":
		return t.browseStatus(ctx, actor)
	case "browseagent":
		return t.browseAgent(ctx, actor, q.Agent)
	case "browseissues":
		return t.browseCollaboration(ctx, actor, q, event.KIND_GIT_ISSUE)
	case "browsepulls":
		return t.browseCollaboration(ctx, actor, q, event.KIND_GIT_PR)
	case "browseissue":
		if q.Event == "" {
			q.Event = q.ID
		}
		return t.browseCollaborationDetail(ctx, actor, q, event.KIND_GIT_ISSUE)
	case "browsepull":
		if q.Event == "" {
			q.Event = q.ID
		}
		return t.browseCollaborationDetail(ctx, actor, q, event.KIND_GIT_PR)
	default:
		return nil, errors.New("unsupported: browse operation")
	}
}

func (t *Tenant) browseRepoAccess(ctx context.Context, actor string, r gitrelay.Repository) error {
	if !t.Policy().Features.Grasp || t.git == nil {
		return errors.New("not found: Git hosting is disabled")
	}
	if !r.Private {
		return nil
	}
	if actor == "" {
		return errors.New("auth-required: private repository")
	}
	role, err := t.community.Role(ctx, actor)
	if err != nil {
		return err
	}
	if role == "owner" || role == "moderator" || role == "member" || t.IsMaintainer(ctx, r, actor) {
		return nil
	}
	return errors.New("restricted: private repository membership required")
}

func (t *Tenant) browseRepos(ctx context.Context, actor string, q clientBrowseRequest) (any, error) {
	if !t.Policy().Features.Grasp || t.git == nil {
		return nil, errors.New("not found: Git hosting is disabled")
	}
	items := []map[string]any{}
	next := ""
	for _, r := range t.git.Repositories() {
		cursor := r.Owner + "/" + r.Identifier
		if cursor <= q.Cursor || (q.Query != "" && !strings.Contains(strings.ToLower(r.Identifier), strings.ToLower(q.Query))) {
			continue
		}
		if err := t.browseRepoAccess(ctx, actor, r); err != nil {
			continue
		}
		if len(items) == q.Limit {
			last := items[len(items)-1]
			next = last["owner"].(string) + "/" + last["identifier"].(string)
			break
		}
		items = append(items, map[string]any{"owner": r.Owner, "identifier": r.Identifier, "head": r.Head, "private": r.Private || t.PrivateServiceEnabled(), "clone": r.Clone, "relays": r.Relays, "refs": r.Refs})
	}
	return map[string]any{"items": items, "next_cursor": next}, nil
}

func (t *Tenant) browseRepo(ctx context.Context, actor string, q clientBrowseRequest) (any, error) {
	if t.git == nil {
		return nil, errors.New("not found: Git hosting")
	}
	r, err := t.git.BrowseRepository(q.Owner, q.Repo)
	if err != nil {
		return nil, err
	}
	if err := t.browseRepoAccess(ctx, actor, r); err != nil {
		return nil, err
	}
	page, err := t.git.Browse(ctx, q.BrowseRequest)
	if err != nil {
		return nil, err
	}
	page.Private = page.Private || t.PrivateServiceEnabled()
	page.Maintainers = t.repositoryMaintainers(ctx, r)
	if q.View != "activity" {
		return page, nil
	}
	rows, err := t.browseRepoEvents(ctx, actor, r, q.Limit)
	return struct {
		gitrelay.BrowsePage
		Events []event.Event `json:"events"`
	}{page, rows}, err
}

func (t *Tenant) browseRepoEvents(ctx context.Context, actor string, r gitrelay.Repository, limit int) ([]event.Event, error) {
	coordinate := "30617:" + r.Owner + ":" + r.Identifier
	kinds := []int{1617, 1618, 1619, 1621, 1111, 1630, 1631, 1632, 1633}
	filters := []event.Filter{}
	for _, tag := range []string{"a", "A"} {
		filters = append(filters, event.Filter{Kinds: kinds, Tags: map[string][]string{tag: {coordinate}}, Limit: &limit})
	}
	session := relay.Session{RelayURL: t.RelayURL()}
	if actor != "" {
		session.PubKeys = []string{actor}
	}
	rows, err := t.Query(ctx, filters, session)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if len(ids) > 0 {
		threads := []event.Filter{}
		for _, tag := range []string{"e", "E"} {
			threads = append(threads, event.Filter{Kinds: kinds, Tags: map[string][]string{tag: ids}, Limit: &limit})
		}
		replies, err := t.Query(ctx, threads, session)
		if err != nil {
			return nil, err
		}
		rows = append(rows, replies...)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt == rows[j].CreatedAt {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt > rows[j].CreatedAt
	})
	seen := map[string]bool{}
	result := []event.Event{}
	for _, row := range rows {
		if !seen[row.ID] {
			seen[row.ID] = true
			result = append(result, row)
		}
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

func (t *Tenant) browseFiles(ctx context.Context, actor string, q clientBrowseRequest) (any, error) {
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
	var entries []blob.Blob
	var next string
	if role == "owner" || role == "moderator" {
		entries, next, err = t.browseAllFiles(ctx, q)
	} else if strings.TrimSpace(q.Query) != "" {
		// ListPage is uploader scoped and cannot filter server-side. Walk its
		// cursor until the requested number of matches is collected.
		cursor := q.Cursor
		for len(entries) < q.Limit {
			var page []blob.Blob
			page, next, err = t.blobs.ListPage(ctx, actor, q.Limit-len(entries), cursor)
			if err != nil {
				break
			}
			for _, entry := range page {
				if blobMatchesQuery(entry, q.Query) {
					entries = append(entries, entry)
					if len(entries) == q.Limit {
						break
					}
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
	for _, entry := range entries {
		if q.Query != "" && !blobMatchesQuery(entry, q.Query) {
			continue
		}
		items = append(items, t.browseBlobMetadata(entry))
	}
	return map[string]any{"items": items, "next_cursor": next}, nil
}

func (t *Tenant) browseAllFiles(ctx context.Context, q clientBrowseRequest) ([]blob.Blob, string, error) {
	query := "SELECT sha256,size,type,uploader,uploaded FROM blobs WHERE sha256>?"
	args := []any{q.Cursor}
	if q.Query != "" {
		query += " AND (instr(lower(sha256), ?) > 0 OR instr(lower(type), ?) > 0 OR instr(lower(uploader), ?) > 0)"
		needle := strings.ToLower(strings.TrimSpace(q.Query))
		args = append(args, needle, needle, needle)
	}
	query += " ORDER BY sha256 LIMIT ?"
	args = append(args, q.Limit+1)
	rows, err := t.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	entries := []blob.Blob{}
	for rows.Next() {
		var entry blob.Blob
		if err := rows.Scan(&entry.SHA256, &entry.Size, &entry.Type, &entry.Uploader, &entry.Uploaded); err != nil {
			return nil, "", err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(entries) > q.Limit {
		entries = entries[:q.Limit]
		next = entries[len(entries)-1].SHA256
	}
	return entries, next, nil
}

func blobMatchesQuery(entry blob.Blob, query string) bool {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(entry.SHA256), needle) ||
		strings.Contains(strings.ToLower(entry.Type), needle) ||
		strings.Contains(strings.ToLower(entry.Uploader), needle)
}

func (t *Tenant) browseBlobMetadata(entry blob.Blob) map[string]any {
	portable := blob.BlossomURI{Hash: entry.SHA256, Extension: "bin", Author: entry.Uploader, Size: entry.Size}
	if origin, err := url.Parse(t.publicURL); err == nil && strings.Trim(origin.Path, "/") == "" {
		portable.Servers = []string{origin.Scheme + "://" + origin.Host}
	}
	return map[string]any{"sha256": entry.SHA256, "size": entry.Size, "type": entry.Type, "uploader": entry.Uploader, "uploaded": entry.Uploaded, "url": strings.TrimRight(t.publicURL, "/") + "/" + entry.SHA256, "download_url": strings.TrimRight(t.publicURL, "/") + "/files/raw?hash=" + url.QueryEscape(entry.SHA256), "blossom_uri": portable.String()}
}

func (t *Tenant) browseFile(ctx context.Context, actor, hash string) (any, error) {
	if !t.Policy().Features.Files {
		return nil, errors.New("not found: files are disabled")
	}
	entry, body, err := t.blobs.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 256*1024+1))
	if err != nil {
		return nil, err
	}
	truncated := len(data) > 256*1024
	if truncated {
		data = data[:256*1024]
	}
	binary := bytes.IndexByte(data, 0) >= 0 || (!utf8.Valid(data) && !truncated)
	result := t.browseBlobMetadata(entry)
	result["binary"], result["truncated"] = binary, truncated
	if entry.Type == "application/octet-stream" {
		if detected := blob.DetectMediaType(data); detected != "application/octet-stream" {
			result["type"] = detected
		}
	}
	if actor == "" {
		delete(result, "uploader")
	}
	if !binary {
		result["content"] = strings.ToValidUTF8(string(data), "�")
	}
	return result, nil
}

// browseAgent returns one agent grant with the agent's newest events for the
// Manage > Agents page. Like listagents, it is for the owner and moderators.
func (t *Tenant) browseAgent(ctx context.Context, actor, agent string) (any, error) {
	role, err := t.community.Role(ctx, actor)
	if err != nil {
		return nil, err
	}
	if role != "owner" && role != "moderator" {
		return nil, errors.New("restricted: agent details")
	}
	if len(agent) != 64 || strings.Trim(agent, "0123456789abcdef") != "" {
		return nil, errors.New("invalid: agent public key required")
	}
	grant, ok, err := t.community.AgentGrant(ctx, agent)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("not found: no agent grant for this pubkey")
	}
	rows, err := t.store.Query(ctx, event.Filter{Authors: []string{agent}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 10})
	if err != nil {
		return nil, err
	}
	summary := community.AgentSummary{AgentGrant: grant}
	if len(rows.Events) > 0 {
		summary.LastEvent = rows.Events[0].CreatedAt
	}
	return map[string]any{"agent": summary, "events": rows.Events}, nil
}

func (t *Tenant) browseStatus(ctx context.Context, actor string) (any, error) {
	role, err := t.community.Role(ctx, actor)
	if err != nil {
		return nil, err
	}
	if role != "owner" && role != "moderator" {
		return nil, errors.New("restricted: operator status")
	}
	result := map[string]any{"capabilities": t.Capabilities(t.app.PeerMonitor()), "git_sync": nil, "jobs": nil, "delivery": nil, "storage": nil, "backups": nil}
	failures := map[string]string{}
	if t.git != nil {
		status, err := t.git.GRASPService().SyncStatus(ctx)
		if err != nil {
			failures["git_sync"] = err.Error()
		} else {
			result["git_sync"] = status
		}
	}
	for section, method := range map[string]string{"jobs": "listjobs", "delivery": "deliverystatus", "storage": "storagestats", "backups": "listbackups"} {
		value, err := t.Execute(ctx, actor, method, nil)
		if err != nil {
			failures[section] = err.Error()
		} else {
			result[section] = value
		}
	}
	if peers := t.app.PeerMonitor(); peers != nil {
		result["peers"] = peers.Snapshot()
	}
	result["errors"] = failures
	return result, nil
}
