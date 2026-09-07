package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
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
	default:
		return nil, nil
	}
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
	return !(path == "/articles.json" || path == "/feed" || path == "/feed.xml" || path == "/articles" || strings.HasPrefix(path, "/e/") || strings.HasPrefix(path, "/a/"))
}

func (a *App) feed(writer http.ResponseWriter, request *http.Request) {
	items, err := a.publicFeed(request.Context(), "articles", "", request.URL.Query())
	if err != nil {
		http.Error(writer, err.Error(), publicErrorStatus(err))
		return
	}
	writer.Header().Set("content-type", "application/atom+xml; charset=utf-8")
	var out strings.Builder
	fmt.Fprintf(&out, `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>%s</title><link href="%s"/>`, template.HTMLEscapeString(a.backend.Slug()), template.HTMLEscapeString(a.backend.URL()))
	for _, value := range items {
		item, ok := articleItem(value, a.backend.URL())
		if !ok {
			continue
		}
		fmt.Fprintf(&out, `<entry><id>%s</id><title>%s</title><link href="%s"/><content type="text">%s</content></entry>`, template.HTMLEscapeString(fmt.Sprint(item["id"])), template.HTMLEscapeString(fmt.Sprint(item["title"])), template.HTMLEscapeString(fmt.Sprint(item["url"])), template.HTMLEscapeString(fmt.Sprint(item["content_text"])))
	}
	out.WriteString(`</feed>`)
	_, _ = writer.Write([]byte(out.String()))
}
