package tinyclient

import (
	"context"
	"encoding/hex"
	"net/url"
	"strings"
)

// NameDirectory resolves relay member names for URLs. It is optional; a
// backend without it writes and reads hex pubkeys only.
type NameDirectory interface {
	// NameFor returns the member name for a pubkey, or "" when it has none.
	NameFor(ctx context.Context, pubkey string) string
	// PubKeyFor returns the pubkey behind a member name, or "" when unknown.
	PubKeyFor(ctx context.Context, name string) string
}

// person turns a URL segment into a pubkey. It accepts a member name, an
// npub and a hex pubkey. An unknown name resolves to "".
func (a *App) person(ctx context.Context, segment string) string {
	segment = strings.TrimSpace(segment)
	if segment == "" {
		return ""
	}
	if eventIDPattern.MatchString(segment) {
		return strings.ToLower(segment)
	}
	if strings.HasPrefix(segment, "npub1") {
		if hrp, data, ok := bech32Decode(segment); ok && hrp == "npub" && len(data) == 32 {
			return hex.EncodeToString(data)
		}
		return ""
	}
	if directory, ok := a.backend.(NameDirectory); ok {
		return directory.PubKeyFor(ctx, segment)
	}
	// Without a directory the segment is passed through as written; the
	// backend decides whether it names anyone.
	return segment
}

// handle is the segment a pubkey is written as: the member name when the
// relay knows one, else the hex pubkey.
func (a *App) handle(ctx context.Context, pubkey string) string {
	if directory, ok := a.backend.(NameDirectory); ok && pubkey != "" {
		if name := directory.NameFor(ctx, pubkey); name != "" {
			return name
		}
	}
	return pubkey
}

// repoRoute is one parsed /repos/{owner}/{repo}/... path. Segments stay as
// written; the owner is resolved to a pubkey by the caller.
type repoRoute struct {
	owner, repo, view, path, id string
	ok                          bool
}

var repoViews = map[string]bool{"tree": true, "file": true, "history": true, "commit": true, "activity": true, "issues": true, "prs": true}

func parseRepoRoute(path string) repoRoute {
	rest, ok := strings.CutPrefix(path, "/repos/")
	if !ok || rest == "" {
		return repoRoute{}
	}
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return repoRoute{}
	}
	route := repoRoute{owner: unescapeSegment(parts[0]), repo: unescapeSegment(parts[1]), view: "home", ok: true}
	if len(parts) == 2 {
		return route
	}
	view := parts[2]
	if !repoViews[view] {
		return repoRoute{}
	}
	route.view = view
	tail := ""
	if len(parts) == 4 {
		tail = parts[3]
	}
	switch view {
	case "tree":
		route.path = unescapePath(tail)
	case "file":
		if tail == "" {
			return repoRoute{}
		}
		route.path = unescapePath(tail)
	case "commit":
		if tail == "" {
			return repoRoute{}
		}
		route.id = unescapeSegment(tail)
	case "issues", "prs":
		if tail != "" {
			if !eventIDPattern.MatchString(tail) {
				return repoRoute{}
			}
			route.id = tail
			route.view = map[string]string{"issues": "issue", "prs": "pr"}[view]
		}
	default:
		if tail != "" {
			return repoRoute{}
		}
	}
	return route
}

func unescapeSegment(value string) string {
	if decoded, err := url.PathUnescape(value); err == nil {
		return decoded
	}
	return value
}

func unescapePath(value string) string {
	parts := strings.Split(value, "/")
	for i, part := range parts {
		parts[i] = unescapeSegment(part)
	}
	return strings.Join(parts, "/")
}

func escapePath(value string) string {
	parts := strings.Split(value, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// repoPath writes a repository page address. owner is the handle to show,
// view one of the page views (issue and pr mean one item named by id), and
// path or id the item within the view. Query keeps only transient state.
func repoPath(owner, repo, view, item string, query url.Values) string {
	base := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	switch view {
	case "", "home":
	case "tree":
		base += "/tree"
		if item != "" {
			base += "/" + escapePath(item)
		}
	case "file":
		base += "/file/" + escapePath(item)
	case "commit":
		base += "/commit/" + url.PathEscape(item)
	case "issue":
		base += "/issues/" + url.PathEscape(item)
	case "pr":
		base += "/prs/" + url.PathEscape(item)
	default:
		base += "/" + view
	}
	keep := url.Values{}
	for _, key := range []string{"ref", "offset", "cursor", "q", "status", "label", "download"} {
		if value := query.Get(key); value != "" {
			keep.Set(key, value)
		}
	}
	if len(keep) == 0 {
		return base
	}
	return base + "?" + keep.Encode()
}

// routeKeys are query values the page derives from its path. Links built
// from the current query drop them so they are never written back out.
var routeKeys = map[string]bool{"owner": true, "handle": true, "repo": true, "view": true, "id": true, "hash": true, "agent": true, "merge": true, "d": true, "address": true}

func transientQuery(query url.Values) url.Values {
	values := make(url.Values, len(query))
	for name, items := range query {
		if routeKeys[name] {
			continue
		}
		values[name] = append([]string(nil), items...)
	}
	return values
}

// filesView names the collection behind /files/{view}, or "" when the path
// is not one.
func filesView(path string) string {
	view, ok := strings.CutPrefix(path, "/files/")
	if !ok {
		return ""
	}
	switch view {
	case "sites", "rooms", "storage":
		return view
	}
	return ""
}

// fileHash is the hash behind /file/{hash}, or "".
func fileHash(path string) string {
	hash, ok := strings.CutPrefix(path, "/file/")
	if !ok || hash == "" || strings.Contains(hash, "/") {
		return ""
	}
	return unescapeSegment(hash)
}

// approvalID is the request behind /approvals/{id}, or "".
func approvalID(path string) string {
	id, ok := strings.CutPrefix(path, "/approvals/")
	if !ok || !eventIDPattern.MatchString(id) {
		return ""
	}
	return strings.ToLower(id)
}

// addHandles writes a "handle" beside the pubkey field of each row so
// templates can link people by name.
func (a *App) addHandles(ctx context.Context, rows []any, field string) {
	for _, row := range rows {
		if item, _ := row.(map[string]any); item != nil {
			if pubkey := plainString(item[field]); pubkey != "" {
				item["handle"] = a.handle(ctx, pubkey)
			}
		}
	}
}
