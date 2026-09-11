package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
)

func (a *App) publicFeed(ctx context.Context, tab, actor string, values url.Values) ([]any, error) {
	if (tab == "inbox" || tab == "outbox") && strings.TrimSpace(actor) == "" {
		return nil, &publicAuthError{}
	}
	var filter map[string]any
	switch tab {
	case "search":
		q := strings.TrimSpace(values.Get("q"))
		if q == "" {
			return nil, nil
		}
		filter = map[string]any{"search": q, "limit": 50}
	case "inbox":
		filter = map[string]any{"#p": []string{actor}, "limit": 50}
	case "outbox":
		filter = map[string]any{"authors": []string{actor}, "limit": 50}
	case "articles":
		filter = map[string]any{"kinds": []int{30023}, "limit": 50}
	case "home":
		filter = map[string]any{"kinds": []int{1, 30023}, "limit": 12}
	case "sites":
		filter = map[string]any{"kinds": []int{sites.KindSite, sites.KindNamedSite}, "limit": 100}
	default:
		return nil, nil
	}
	applyFeedFilters(filter, values)
	raw, err := json.Marshal(filter)
	if err != nil {
		return nil, err
	}
	result, err := a.backend.Query(ctx, "queryevents", []json.RawMessage{raw}, actor)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var feed []any
	if err := json.Unmarshal(encoded, &feed); err != nil {
		return []any{}, nil
	}
	return feed, nil
}

// applyFeedFilters narrows a feed by the panel filters: kinds, author, since.
// "all" removes the kind restriction; unknown values are ignored.
func applyFeedFilters(filter map[string]any, values url.Values) {
	switch kinds := strings.TrimSpace(values.Get("kinds")); {
	case kinds == "all":
		delete(filter, "kinds")
	case kinds != "":
		var list []int
		for _, part := range strings.Split(kinds, ",") {
			if kind, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
				list = append(list, kind)
			}
		}
		if len(list) > 0 {
			filter["kinds"] = list
		}
	}
	if author := strings.ToLower(strings.TrimSpace(values.Get("author"))); len(author) == 64 {
		filter["authors"] = []string{author}
	}
	if since := strings.TrimSpace(values.Get("since")); since != "" {
		if parsed, err := time.Parse("2006-01-02", since); err == nil {
			filter["since"] = parsed.Unix()
		}
	}
}

// siteRows turns site manifest events into rows with the host each site is
// served from, using the same labels the daemon uses for site hosts.
func (a *App) siteRows(feed []any) []any {
	base, err := url.Parse(a.backend.URL())
	if err != nil {
		return nil
	}
	rows := make([]any, 0, len(feed))
	for _, item := range feed {
		encoded, _ := json.Marshal(item)
		var e event.Event
		if json.Unmarshal(encoded, &e) != nil {
			continue
		}
		label := sites.SiteLabel(e)
		if label == "" {
			continue
		}
		scheme := base.Scheme
		if scheme == "" || scheme == "ws" {
			scheme = "http"
		} else if scheme == "wss" {
			scheme = "https"
		}
		rows = append(rows, map[string]any{"id": e.ID, "kind": e.Kind, "author": e.PubKey, "label": label, "paths": len(sites.SitePaths(e)), "created_at": e.CreatedAt, "url": scheme + "://" + label + "." + base.Host})
	}
	return rows
}

type publicAuthError struct{}

func (*publicAuthError) Error() string { return "authentication required" }

func publicErrorStatus(err error) int {
	if _, ok := err.(*publicAuthError); ok {
		return http.StatusUnauthorized
	}
	return http.StatusForbidden
}

func (a *App) articlesJSON(writer http.ResponseWriter, request *http.Request) {
	feed, err := a.publicFeed(request.Context(), "articles", "", request.URL.Query())
	if err != nil {
		writeJSON(writer, publicErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	items := make([]map[string]any, 0, len(feed))
	for _, value := range feed {
		item, ok := articleItem(value, a.backend.URL())
		if ok {
			items = append(items, item)
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"version": "https://jsonfeed.org/version/1", "title": a.backend.Slug(), "home_page_url": a.backend.URL(), "feed_url": strings.TrimSuffix(a.backend.URL(), "/") + "/articles.json", "items": items})
}

func articleItem(value any, base string) (map[string]any, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var event map[string]any
	if json.Unmarshal(encoded, &event) != nil {
		return nil, false
	}
	id, _ := event["id"].(string)
	content, _ := event["content"].(string)
	if id == "" {
		return nil, false
	}
	title := strings.TrimSpace(strings.SplitN(content, "\n", 2)[0])
	if title == "" {
		title = "Article " + id[:minInt(12, len(id))]
	}
	item := map[string]any{"id": id, "url": strings.TrimSuffix(base, "/") + "/e/" + id, "title": title, "content_text": content}
	if created, ok := event["created_at"].(float64); ok && created > 0 {
		item["date_published"] = time.Unix(int64(created), 0).UTC().Format(time.RFC3339)
	}
	return item, true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (a *App) pageFeatureEnabled(path string) bool {
	if a.backend.Policy().Features.Pages {
		return true
	}
	return !(path == "/social" || path == "/social.json" || path == "/social.xml" || strings.HasPrefix(path, "/social/") || path == "/articles.json" || path == "/feed" || path == "/feed.xml" || path == "/articles" || strings.HasPrefix(path, "/e/") || strings.HasPrefix(path, "/a/"))
}

func (a *App) feed(writer http.ResponseWriter, request *http.Request) {
	items, err := a.publicFeed(request.Context(), "articles", "", request.URL.Query())
	if err != nil {
		http.Error(writer, err.Error(), publicErrorStatus(err))
		return
	}
	entries := make([]map[string]any, 0, len(items))
	for _, value := range items {
		if item, ok := articleItem(value, a.backend.URL()); ok {
			entries = append(entries, item)
		}
	}
	a.writeAtom(writer, entries)
}

func (a *App) writeAtom(writer http.ResponseWriter, items []map[string]any) {
	writer.Header().Set("content-type", "application/atom+xml; charset=utf-8")
	var out strings.Builder
	fmt.Fprintf(&out, `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>%s</title><link href="%s"/>`, template.HTMLEscapeString(a.backend.Slug()), template.HTMLEscapeString(a.backend.URL()))
	for _, item := range items {
		fmt.Fprintf(&out, `<entry><id>%s</id><title>%s</title><link href="%s"/><content type="text">%s</content></entry>`, template.HTMLEscapeString(fmt.Sprint(item["id"])), template.HTMLEscapeString(fmt.Sprint(item["title"])), template.HTMLEscapeString(fmt.Sprint(item["url"])), template.HTMLEscapeString(fmt.Sprint(item["content_text"])))
	}
	out.WriteString(`</feed>`)
	_, _ = writer.Write([]byte(out.String()))
}
