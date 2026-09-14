package tinyclient

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type podcastRSS struct {
	XMLName xml.Name       `xml:"rss"`
	Version string         `xml:"version,attr"`
	Atom    string         `xml:"xmlns:atom,attr"`
	Channel podcastChannel `xml:"channel"`
}

type podcastChannel struct {
	Title       string          `xml:"title"`
	Link        string          `xml:"link"`
	Description string          `xml:"description"`
	Self        podcastAtomLink `xml:"atom:link"`
	Items       []podcastItem   `xml:"item"`
}

type podcastAtomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type podcastItem struct {
	Title       string           `xml:"title"`
	Link        string           `xml:"link"`
	Description string           `xml:"description"`
	GUID        podcastGUID      `xml:"guid"`
	PubDate     string           `xml:"pubDate,omitempty"`
	Enclosure   podcastEnclosure `xml:"enclosure"`
}

type podcastGUID struct {
	Value       string `xml:",chardata"`
	IsPermaLink string `xml:"isPermaLink,attr"`
}

type podcastEnclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

// podcastRSS exposes Social audio posts to conventional podcast readers.
func (a *App) podcastRSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	actor, err := a.resolveActor(r)
	if err != nil {
		http.Error(w, err.Error(), socialErrorStatus(err))
		return
	}
	query, err := podcastFeedQuery(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	params := socialParameters(query)
	params["kind"], params["limit"] = "podcasts", 100
	raw, err := json.Marshal(params)
	if err != nil {
		http.Error(w, "invalid social query", http.StatusBadRequest)
		return
	}
	result, err := a.backend.Query(r.Context(), "browsesocial", []json.RawMessage{raw}, actor)
	if err != nil {
		http.Error(w, err.Error(), socialErrorStatus(err))
		return
	}
	base := strings.TrimSuffix(a.backend.URL(), "/")
	channel := podcastFeedChannel(base, a.backend.Slug(), query)
	for _, row := range browseRows(result) {
		if item, ok := podcastFeedItem(base, valueMap(row)); ok {
			channel.Items = append(channel.Items, item)
		}
	}
	doc, err := xml.Marshal(podcastRSS{Version: "2.0", Atom: "http://www.w3.org/2005/Atom", Channel: channel})
	if err != nil {
		http.Error(w, "could not encode feed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(xml.Header)+len(doc)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(append([]byte(xml.Header), doc...))
	}
}

func podcastFeedQuery(query url.Values) (url.Values, error) {
	values := url.Values{}
	if raw := strings.TrimSpace(query.Get("author")); raw != "" {
		author, err := socialProfileAuthor(raw)
		if err != nil {
			return nil, err
		}
		values.Set("author", author)
	}
	if search := query.Get("q"); search != "" {
		values.Set("q", search)
	}
	return values, nil
}

func podcastFeedChannel(base, slug string, query url.Values) podcastChannel {
	channel := podcastChannel{
		Title:       slug + " Podcasts",
		Link:        base + "/social?kind=podcasts",
		Description: "Audio posts shared on " + slug + ".",
		Self:        podcastAtomLink{Href: base + podcastFeedURL(query), Rel: "self", Type: "application/rss+xml"},
	}
	if author := query.Get("author"); author != "" {
		channel.Title = shortID(author) + " Podcasts | " + slug
		channel.Link = base + "/social/profile?author=" + url.QueryEscape(author) + "&kind=podcasts"
	}
	return channel
}

func podcastFeedItem(base string, row map[string]any) (podcastItem, bool) {
	for _, audio := range socialAttachments(row) {
		if !strings.HasPrefix(strings.ToLower(audio.MIME), "audio/") || socialMediaURL(audio.URL) == "" {
			continue
		}
		itemURL := base + socialURL(row)
		item := podcastItem{
			Title:       podcastEpisodeTitle(row, audio.Name),
			Link:        itemURL,
			Description: plainString(row["content"]),
			GUID:        podcastGUID{Value: plainString(row["id"]), IsPermaLink: "false"},
			Enclosure:   podcastEnclosure{URL: audio.URL, Type: audio.MIME, Length: audio.Size},
		}
		if item.GUID.Value == "" {
			item.GUID.Value = itemURL
		}
		if address := plainString(row["address"]); address != "" {
			item.GUID.Value = address
		}
		if seconds := unixSeconds(row["created_at"]); seconds > 0 {
			item.PubDate = time.Unix(seconds, 0).UTC().Format(time.RFC1123Z)
		}
		return item, true
	}
	return podcastItem{}, false
}

func podcastEpisodeTitle(row map[string]any, filename string) string {
	if title := strings.TrimSpace(plainString(row["title"])); title != "" {
		return title
	}
	content := strings.TrimSpace(plainString(row["content"]))
	line, _, _ := strings.Cut(content, "\n")
	if line != "" && socialMediaURL(line) == "" {
		return socialPreview(line)
	}
	if filename != "" {
		return filename
	}
	return "Untitled podcast"
}
