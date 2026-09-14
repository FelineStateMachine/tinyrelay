package tinyclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/podcasts"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

type nativePodcastBackend struct{ fakeBackend }

func (b *nativePodcastBackend) Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	key := strings.Repeat("a", 64)
	show := podcasts.Show{ID: "metadata", Pubkey: key, Title: "Mountain Radio", Description: "Stories from the trail", Image: "https://example.test/show.jpg", Websites: []string{"https://example.test"}, Tags: [][]string{{"title", "Mountain Radio"}}}
	post := map[string]any{"id": strings.Repeat("b", 64), "kind": 54, "pubkey": key, "title": "First light", "created_at": 1789000000, "content": "## Field notes\n\nA **quiet** morning.", "tags": [][]string{{"title", "First light"}, {"description", "An early start"}, {"audio", "https://example.test/first.mp3", "audio/mpeg"}}, "podcast": show, "author_name": show.Title}
	switch method {
	case "browsesocial", "browsepodcasts":
		return map[string]any{"show": show, "items": []any{post}}, nil
	case "browsesocialthread":
		return map[string]any{"post": post}, nil
	case "browseprofile":
		return map[string]any{"profile": map[string]any{"name": "Personal profile"}}, nil
	default:
		return b.fakeBackend.Query(ctx, method, params, actor)
	}
}

func TestNativePodcastPagesRenderAudioAndShowIdentity(t *testing.T) {
	key := strings.Repeat("a", 64)
	b := &nativePodcastBackend{fakeBackend{policy: policy.Defaults(key)}}
	a, err := New(b, Options{Actor: func(*http.Request) (string, error) { return key, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/social?kind=podcasts&author=" + key, "/social/profile?kind=podcasts&author=" + key, "/social/" + strings.Repeat("b", 64)} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		body := w.Body.String()
		for _, want := range []string{"Mountain Radio", "First light", `<audio controls`, `src="https://example.test/first.mp3"`, `data-post-body="podcast"`} {
			if w.Code != 200 || !strings.Contains(body, want) {
				t.Errorf("%s missing %q: %d", path, want, w.Code)
			}
		}
		if strings.Contains(path, "/social/"+strings.Repeat("b", 64)) && (!strings.Contains(body, "Field notes</h2>") || !strings.Contains(body, "<strong>quiet</strong>")) {
			t.Error("Podcast show notes are not Markdown")
		}
		if strings.Contains(path, "profile?") && strings.Contains(body, "Personal profile") {
			t.Error("Podcast profile used personal metadata instead of show")
		}
	}
}

func TestPodcastComposerUsesNativeEpisodeAndShowModes(t *testing.T) {
	key := strings.Repeat("a", 64)
	a, err := New(&nativePodcastBackend{fakeBackend{policy: policy.Defaults(key)}}, Options{Actor: func(*http.Request) (string, error) { return key, nil }})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/social?kind=podcasts&author="+key, nil))
	for _, want := range []string{`mode="podcast"`, `mode="podcast-show"`, `show-tags=`, `accept="audio/*"`, `Publish episode`, `Save podcast details`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestSocialPodcastEventKeepsTheNativeKind(t *testing.T) {
	for _, kind := range []int{1, 30023, 54} {
		e := socialPodcastEvent(map[string]any{"kind": kind, "tags": [][]string{{"audio", "https://example.test/ep.mp3"}}})
		_, ok := podcasts.ParseEpisode(e)
		if ok != (kind == 54) {
			t.Errorf("kind %d parsed as podcast: %v", kind, ok)
		}
	}
	native := event.Event{Kind: 54, ID: "native", Tags: [][]string{{"audio", "https://example.test/ep.mp3"}}}
	if got := socialPodcastEvent(map[string]any{"event": native}); got.ID != "native" {
		t.Fatalf("nested event: %#v", got)
	}
}
