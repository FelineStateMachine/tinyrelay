package tinyclient

import "net/url"

type socialViewItem struct {
	Key, Label, Summary, URL string
}

func socialViews() []socialViewItem {
	return []socialViewItem{
		{"feed", "Feed", "Notes, posts and media, together in one place.", "/social"},
		{"notes", "Notes", "Quick thoughts and everyday updates.", "/social?kind=notes"},
		{"posts", "Posts", "Stories, essays and longer reads.", "/social?kind=posts"},
		{"photos", "Photos", "Pictures and the stories behind them.", "/social?kind=photos"},
		{"videos", "Videos", "Clips and films shared in posts.", "/social?kind=videos"},
		{"podcasts", "Podcasts", "Shows and episodes, ready for your favorite player.", "/social?kind=podcasts"},
	}
}

func socialView(query url.Values) socialViewItem {
	kind := query.Get("kind")
	if kind == "" {
		switch query.Get("kinds") {
		case "1":
			kind = "notes"
		case "30023":
			kind = "posts"
		}
	}
	if kind == "articles" {
		kind = "posts"
	}
	for _, view := range socialViews() {
		if view.Key == kind {
			return view
		}
	}
	return socialViews()[0]
}

func socialFeedPage(data PageData, cursor string) string {
	path := "/social"
	if data.Tab == "social-profile" {
		path = "/social/profile"
	}
	query := url.Values{}
	for _, key := range []string{"kind", "kinds", "author", "q"} {
		if value := data.Query.Get(key); value != "" {
			query.Set(key, value)
		}
	}
	query.Set("cursor", cursor)
	return path + "?" + query.Encode()
}

func podcastFeedURL(query url.Values) string {
	values := url.Values{}
	for _, key := range []string{"author", "q"} {
		if value := query.Get(key); value != "" {
			values.Set(key, value)
		}
	}
	path := "/social/podcasts.rss"
	if len(values) > 0 {
		path += "?" + values.Encode()
	}
	return path
}
