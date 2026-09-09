package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/views"
	"github.com/FelineStateMachine/tinyrelay/internal/wiki"
)

// viewsBackend reports two custom views and lists them for the owner.
type viewsBackend struct {
	fakeBackend
	views []views.View
	rows  []map[string]any
}

func (b *viewsBackend) CustomViews() []views.View { return b.views }

func (b *viewsBackend) Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	if method == "listcustomviews" {
		if actor != b.policy.Owner {
			return nil, context.Canceled
		}
		return b.rows, nil
	}
	if method == "browserepo" {
		var request map[string]any
		if len(params) == 1 {
			_ = json.Unmarshal(params[0], &request)
		}
		if request["view"] == "file" {
			return map[string]any{"content": "# Notes\n\n```mermaid\ngraph TD; a-->b;\n```\n\nSee [guide](docs/guide.md).\n"}, nil
		}
		return map[string]any{"identifier": "notes", "entries": []any{}}, nil
	}
	return b.fakeBackend.Query(ctx, method, params, actor)
}

const mermaidSource = "graph TD;\n  A-->B & <C>;"

func mermaidFigure(view string) string {
	return `<figure data-view="` + view + `"><object data="/views/` + view + `/` + views.Hash("mermaid", mermaidSource) + `.svg" type="image/svg+xml"><pre><code data-lang="mermaid">graph TD;` + "\n" + `  A--&gt;B &amp; &lt;C&gt;;</code></pre></object></figure>`
}

func TestRenderersShowCustomViewArtifactsWithTheCodeAsFallback(t *testing.T) {
	renderer := views.NewRenderer([]views.View{{Name: "diagrams", Languages: []string{"mermaid"}}})
	source := "Intro\n\n```mermaid\n" + mermaidSource + "\n```\n\n```go\nfunc main() {}\n```\n\nhttps://example.com/x\n"
	got := string(renderMarkdownWith(source, renderer.Block))
	wantAll(t, "markdown", got, mermaidFigure("diagrams"), `<pre><code data-lang="go">func main() {}</code></pre>`)
	if strings.Contains(got, "class=") {
		t.Fatal("markdown carries a class attribute")
	}
	// Without a renderer, every fenced block stays code.
	plain := string(renderMarkdown(source))
	wantAll(t, "plain markdown", plain, `<pre><code data-lang="mermaid">graph TD;`)
	wantNone(t, "plain markdown", plain, "<figure", "<object")
	// Chat keeps the figure and links the bare URL outside it.
	chat := string(renderChatMarkdownWith(source, renderer.Block))
	wantAll(t, "chat", chat, mermaidFigure("diagrams"), `<a href="https://example.com/x" rel="noopener">https://example.com/x</a>`)
	// README rendering maps relative links and keeps the figure.
	query := url.Values{"owner": {"alice"}, "repo": {"notes"}, "ref": {"main"}}
	repo := string(renderRepositoryMarkdownWith(source+"[guide](docs/guide.md)\n", query, renderer.Block))
	wantAll(t, "repository", repo, mermaidFigure("diagrams"), `href="/repo?owner=alice&amp;path=docs%2Fguide.md&amp;ref=main&amp;repo=notes&amp;view=file"`)
	// The wiki renderer takes the same renderer through its options.
	article := string(wiki.RenderHTMLWith("# Title\n\n``` Mermaid\n"+mermaidSource+"\n```\n\n~~~\nplain\n~~~\n", wiki.Options{Block: renderer.Block}))
	wantAll(t, "wiki", article, "<h1>Title</h1>", mermaidFigure("diagrams"), "<pre><code>plain\n</code></pre>")
	if plainWiki := string(wiki.RenderHTML("```mermaid\n" + mermaidSource + "\n```\n")); strings.Contains(plainWiki, "<figure") || !strings.Contains(plainWiki, `<code data-lang="mermaid">`) {
		t.Fatalf("wiki without options: %s", plainWiki)
	}
}

func TestPagesRenderCustomViewFiguresFromTheBackendSummary(t *testing.T) {
	owner := strings.Repeat("a", 64)
	backend := &viewsBackend{fakeBackend: fakeBackend{policy: policy.Defaults(owner)}, views: []views.View{{Name: "diagrams", Languages: []string{"mermaid"}}}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	source := "```mermaid\n" + mermaidSource + "\n```"
	for name, got := range map[string]string{
		"markdown": string(app.markdown(source)),
		"chat":     string(app.chatMarkdown(source)),
		"wiki":     string(app.wikiHTML(source)),
	} {
		wantAll(t, name, got, mermaidFigure("diagrams"))
	}
	// The repository home renders the README through the same renderer,
	// and the tenant prefix reaches the object address.
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/repo?owner="+owner+"&repo=notes&view=home", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("repo home: %d %s", recorder.Code, body)
	}
	hash := views.Hash("mermaid", "graph TD; a-->b;")
	figure := `<figure data-view="diagrams"><object data="/views/diagrams/` + hash + `.svg" type="image/svg+xml"><pre><code data-lang="mermaid">graph TD; a--&gt;b;</code></pre></object></figure>`
	wantAll(t, "repo home", body, figure, `href="/repo?owner=`)
	if prefixed := injectBase(figure, "/r/work"); !strings.Contains(prefixed, `<object data="/r/work/views/diagrams/`+hash+`.svg"`) {
		t.Fatalf("tenant prefix missed the object address: %s", prefixed)
	}
	// A backend without the summary keeps code blocks as code.
	plainApp, err := New(&fakeBackend{policy: policy.Defaults(owner)}, Options{Actor: func(*http.Request) (string, error) { return owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(plainApp.markdown(source)); strings.Contains(got, "<figure") {
		t.Fatalf("figure without a summary: %s", got)
	}
	backend.views = nil
	if got := string(app.markdown(source)); strings.Contains(got, "<figure") {
		t.Fatalf("figure without views: %s", got)
	}
}

func TestViewsPageListsCustomViewsWithControlsAndTheAddForm(t *testing.T) {
	owner := strings.Repeat("a", 64)
	backend := &viewsBackend{fakeBackend: fakeBackend{policy: policy.Defaults(owner)}, rows: []map[string]any{
		{"name": "diagrams", "kinds": []any{1.0, 30023.0}, "languages": []any{"mermaid", "dot"}, "trigger": "write", "audience": "public", "enabled": true, "state": "active", "lastRun": 1700000000.0, "lastStatus": "ok", "transform": "https://render.example/tiny?token=secret-path"},
		{"name": "charts", "kinds": []any{30618.0}, "languages": []any{"vega"}, "trigger": "hourly", "audience": "members", "enabled": false, "state": "paused", "lastRun": 0.0, "lastStatus": "paused after 20 failures: HTTP 500"},
	}}
	app, err := New(backend, Options{Actor: func(r *http.Request) (string, error) { return r.Header.Get("X-Actor"), nil }})
	if err != nil {
		t.Fatal(err)
	}
	get := func(actor string) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/manage/views", nil)
		request.Header.Set("X-Actor", actor)
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("views page: %d %s", recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		if strings.Contains(body, "class=") {
			t.Fatal("views page carries a class attribute")
		}
		if strings.Contains(body, "·") {
			t.Fatal("views page contains a middle dot")
		}
		return body
	}
	body := get(owner)
	wantAll(t, "/manage/views", body,
		`<rpc-form method="listviews">`, `<rpc-form method="publishview">`,
		`<table id="custom-views">`, `<tr id="view-diagrams">`, `<code>diagrams</code>`, `<td>1, 30023</td>`, `<td>mermaid, dot</td>`, `<td>write</td>`, `<td>public</td>`,
		`<td data-state="active">active | ok</td>`, `<td>2023-11-14 22:13 UTC</td>`,
		`<rpc-form method="pausecustomview" refresh><input type="hidden" name="param" value="diagrams"><button>Pause</button></rpc-form>`,
		`<rpc-form method="runcustomview" refresh><input type="hidden" name="param" value="diagrams"><button>Run now</button></rpc-form>`,
		`<rpc-form method="removecustomview" refresh><input type="hidden" name="param" value="diagrams"><button>Remove</button></rpc-form>`,
		`<tr id="view-charts">`, `<td data-state="paused">paused | paused after 20 failures: HTTP 500</td>`, `<td>never</td>`,
		`<rpc-form method="resumecustomview" refresh><input type="hidden" name="param" value="charts"><button>Resume</button></rpc-form>`,
		`<view-form method="addcustomview">`, `<input name="view-name" pattern="[a-z0-9-]{1,32}"`, `<input name="view-kinds"`, `<input name="view-languages"`, `<input name="view-transform" type="url"`,
		`<select name="view-trigger"><option value="write">write</option><option value="hourly">hourly</option></select>`, `<select name="view-audience">`, `<input name="view-max-bytes" type="number" min="1" max="4194304"`, `<input name="view-secret" minlength="16" maxlength="128"`, `<button>Add view</button>`)
	wantNone(t, "/manage/views", body, "secret-path", `<rpc-form method="pausecustomview" refresh><input type="hidden" name="param" value="charts">`)
	// A member keeps the built-in controls and sees no custom view section.
	member := get(strings.Repeat("b", 64))
	wantAll(t, "/manage/views as member", member, `<rpc-form method="listviews">`)
	wantNone(t, "/manage/views as member", member, `id="custom-views"`, "<view-form")
	// The owner without views sees the empty note and the form.
	backend.rows = nil
	empty := get(owner)
	wantAll(t, "/manage/views empty", empty, `<p id="custom-views-empty">No custom views yet.</p>`, `<view-form method="addcustomview">`)
	wantNone(t, "/manage/views empty", empty, `<table id="custom-views">`)
}
