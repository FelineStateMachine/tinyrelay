package tinyclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// BackendPath is the read-only adapter endpoint mounted by App. It uses the
// relay's normal actor resolver; a client-supplied public key is never trusted.
const BackendPath = "/api/tinyclient"

type remoteSnapshot struct {
	Policy        Policy       `json:"policy"`
	URL           string       `json:"url"`
	Slug          string       `json:"slug"`
	Identity      string       `json:"identity"`
	Actor         string       `json:"actor"`
	Version       string       `json:"version"`
	Revision      string       `json:"revision"`
	Views         []CustomView `json:"views,omitempty"`
	SharePresence bool         `json:"share_presence"`
	ReadError     string       `json:"read_error,omitempty"`
	Rooms         bool         `json:"rooms"`
	Activity      bool         `json:"activity"`
}

// BackendHandler exposes the read contract without rendering HTML. Mount it
// at BackendPath when embedding the client separately from its data service.
// Query authorization remains the responsibility of Backend.Query.
func (a *App) BackendHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", 405)
			return
		}
		actor, err := a.resolveActor(r)
		if err != nil {
			http.Error(w, "authentication required", 401)
			return
		}
		if r.URL.Query().Get("op") != "" {
			a.typedBackendRead(w, r, actor)
			return
		}
		if r.URL.Query().Get("method") == "" {
			snapshot := remoteSnapshot{Policy: a.backend.Policy(), URL: a.backend.URL(), Slug: a.backend.Slug(), Identity: a.backend.Identity(), Actor: actor, Version: a.version, Revision: a.revision}
			_, snapshot.Rooms = a.backend.(RoomsReader)
			_, snapshot.Activity = a.backend.(ChatActivityReader)
			if snapshot.Policy.Features.Grasp08 {
				if err := a.privateReadAllowed(r, actor); err != nil {
					snapshot.Policy = Policy{Features: Features{Grasp08: true}}
					snapshot.URL, snapshot.Slug, snapshot.Identity = "", "private relay", ""
					snapshot.ReadError = err.Error()
					writeJSON(w, 200, snapshot)
					return
				}
			}
			if actor == "" || actor != snapshot.Policy.Owner {
				p := snapshot.Policy
				snapshot.Policy = Policy{Owner: p.Owner, Name: p.Name, Description: p.Description, Icon: p.Icon, Banner: p.Banner, Contact: p.Contact, PostingPolicy: p.PostingPolicy, PrivacyPolicy: p.PrivacyPolicy, Tags: p.Tags, LanguageTags: p.LanguageTags, RelayCountries: p.RelayCountries, Reads: p.Reads, Writes: p.Writes, JoinTerms: p.JoinTerms, DirectoryPublic: p.DirectoryPublic, Features: p.Features, Rooms: p.Rooms, Callbacks: p.Callbacks, MemberInvites: p.MemberInvites, FileLimits: p.FileLimits}
			}
			if views, ok := a.backend.(CustomViewSource); ok {
				snapshot.Views = views.CustomViews()
			}
			if actor != "" {
				if preferences, ok := a.backend.(AccountPreferencesReader); ok {
					snapshot.SharePresence = preferences.SharePresence(r.Context(), actor)
				}
			}
			writeJSON(w, 200, snapshot)
			return
		}
		method := r.URL.Query().Get("method")
		if !remoteReadMethods[method] {
			http.Error(w, "unsupported client read", 400)
			return
		}
		if a.backend.Policy().Features.Grasp08 {
			if err := a.privateReadAllowed(r, actor); err != nil {
				http.Error(w, err.Error(), 403)
				return
			}
		}
		var params []json.RawMessage
		if raw := r.URL.Query().Get("params"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &params); err != nil {
				http.Error(w, "invalid client query", 400)
				return
			}
		}
		result, err := a.backend.Query(r.Context(), method, params, actor)
		if err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
		writeJSON(w, 200, result)
	})
}

var remoteReadMethods = map[string]bool{
	"connections": true, "queryevents": true, "eventdetail": true, "listview": true,
	"listpeople": true, "listjoinrequests": true, "listagents": true, "listcallbacks": true,
	"listconnections": true, "listconnectiontemplates": true, "listcustomviews": true, "listjobs": true,
	"browserepos": true, "browserepo": true, "browsefiles": true, "browsefile": true,
	"browseapprovals": true, "browseapproval": true, "browseprofile": true, "browsewiki": true,
	"browsewikipage": true, "browsewikimerge": true, "browsestatus": true,
	"browserooms": true, "browseroom": true, "browsethread": true,
	"browseissues": true, "browseissue": true, "browsepulls": true, "browsepull": true,
	"browseagent": true, "browsejobs": true, "browsejob": true,
	"browsesocial": true, "browsesocialthread": true, "browsepodcasts": true, "browsedirectmessages": true,
}

// RemoteOptions connects a standalone renderer to one relay tenant. PublicURL
// is the browser-facing URL; BackendURL is its upstream HTTP address. Use the
// same tenant path at both addresses so cookies and signed URLs retain scope.
type RemoteOptions struct {
	BackendURL string
	PublicURL  string
	// Transport optionally supplies TLS roots or other HTTP transport settings.
	Transport http.RoundTripper
}

type remoteClient struct {
	target, public *url.URL
	client         *http.Client
	proxy          *httputil.ReverseProxy
}

type requestBackend struct {
	remote   *remoteClient
	snapshot remoteSnapshot
	cookie   string
}

func (b *requestBackend) Policy() Policy                             { return b.snapshot.Policy }
func (b *requestBackend) URL() string                                { return b.snapshot.URL }
func (b *requestBackend) Slug() string                               { return b.snapshot.Slug }
func (b *requestBackend) Identity() string                           { return b.snapshot.Identity }
func (b *requestBackend) CustomViews() []CustomView                  { return b.snapshot.Views }
func (b *requestBackend) SharePresence(context.Context, string) bool { return b.snapshot.SharePresence }
func (b *requestBackend) ReadAllowed(context.Context, string) error {
	if b.snapshot.ReadError != "" {
		return errors.New(b.snapshot.ReadError)
	}
	return nil
}
func (b *requestBackend) Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var result any
	err = b.readAsActor(ctx, actor, url.Values{"method": {method}, "params": {string(raw)}}, &result)
	return result, err
}

func (b *requestBackend) readAsActor(ctx context.Context, actor string, query url.Values, out any) error {
	if actor != "" && actor != b.snapshot.Actor {
		return errors.New("client read actor does not match authenticated session")
	}
	cookie := b.cookie
	// Public reads must remain anonymous even on an authenticated page request.
	if actor == "" {
		cookie = ""
	}
	return b.remote.read(ctx, cookie, query, out)
}

// NewRemote serves embedded templates and assets locally and forwards relay
// protocols, signed requests and mutations upstream. It does not open a
// database, own relay keys or start relay workers.
func NewRemote(options RemoteOptions) (http.Handler, error) {
	parse := func(raw string) (*url.URL, error) {
		u, err := url.Parse(raw)
		if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
			return nil, fmt.Errorf("tinyclient requires an absolute HTTP(S) URL without credentials, query or fragment")
		}
		u.Path = strings.TrimRight(u.Path, "/")
		return u, nil
	}
	target, err := parse(options.BackendURL)
	if err != nil {
		return nil, err
	}
	public, err := parse(options.PublicURL)
	if err != nil {
		return nil, err
	}
	if target.Path != public.Path {
		return nil, errors.New("tinyclient backend and public URL must use the same tenant path")
	}
	if public.Path != "" && pathPrefix(public.Path) != public.Path {
		return nil, errors.New("tinyclient tenant path must be /r/<name>")
	}
	transport := options.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	remote := &remoteClient{target: target, public: public, client: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	remote.proxy = &httputil.ReverseProxy{Transport: transport, Rewrite: func(p *httputil.ProxyRequest) {
		p.Out.URL.Scheme, p.Out.URL.Host = target.Scheme, target.Host
		p.Out.Host = public.Host
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "relay backend unavailable", http.StatusBadGateway)
	}}
	return remote, nil
}

func (remote *remoteClient) read(ctx context.Context, cookie string, query url.Values, out any) error {
	target := *remote.target
	target.Path += BackendPath
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", target.String(), nil)
	if err != nil {
		return err
	}
	req.Host = remote.public.Host
	req.Header.Set("Cookie", cookie)
	response, err := remote.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("%s", strings.TrimSpace(string(raw)))
	}
	return json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(out)
}

func (remote *remoteClient) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Host, remote.public.Host) {
		http.Error(w, "unexpected public host", http.StatusMisdirectedRequest)
		return
	}
	path := r.URL.Path
	prefix := remote.public.Path
	if prefix == "" && strings.HasPrefix(path, "/r/") {
		http.NotFound(w, r)
		return
	}
	if prefix != "" {
		if !strings.HasPrefix(path, prefix+"/") && path != prefix {
			http.NotFound(w, r)
			return
		}
		path = strings.TrimPrefix(path, prefix)
		if path == "" {
			http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
			return
		}
	}
	if r.Method != "GET" && r.Method != "HEAD" || r.Header.Get("Authorization") != "" || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || strings.Contains(r.Header.Get("Accept"), "application/nostr+json") || !clientPath(path) {
		remote.proxy.ServeHTTP(w, r)
		return
	}
	if serveEmbeddedAsset(w, r, path) {
		return
	}
	b := &requestBackend{remote: remote, cookie: r.Header.Get("Cookie")}
	if err := remote.read(r.Context(), b.cookie, nil, &b.snapshot); err != nil {
		http.Error(w, "relay backend unavailable", http.StatusBadGateway)
		return
	}
	b.snapshot.URL = remote.public.String()
	var backend Backend = b
	if b.snapshot.Rooms {
		backend = &remoteRooms{b}
	}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return b.snapshot.Actor, nil }, Version: b.snapshot.Version, Revision: b.snapshot.Revision})
	if err != nil {
		http.Error(w, "client renderer unavailable", 500)
		return
	}
	local := r.Clone(r.Context())
	local.URL.Path = path
	// Preserve RequestURI so prefix-aware templates and browser requests agree.
	if local.RequestURI == "" {
		local.RequestURI = r.URL.RequestURI()
	}
	if r.Method == "HEAD" {
		local.Method = "GET"
		app.ServeHTTP(headWriter{w}, local)
		return
	}
	app.ServeHTTP(w, local)
}

type headWriter struct{ http.ResponseWriter }

func (w headWriter) Write(p []byte) (int, error) { return len(p), nil }

func clientPath(path string) bool {
	if path == "/repo/raw" || path == "/files/raw" || path == "/chat/preferences" || path == "/manage/jobs/status" || path == BackendPath {
		return false
	}
	if tabForPath(path) != "home" || isBrowsePath(path) {
		return true
	}
	switch path {
	case "/", "/card.json", "/people", "/connect.json", "/signer.js", "/fixi.js", "/sw.js", "/icon.svg", "/icon-mono.svg", "/manifest.webmanifest", "/qr.svg", "/card.svg", "/card.nostr", "/webmcp.js", "/webmcp/query", "/api/join-policy", "/share", "/tools", "/manage/status", "/articles.json", "/feed", "/feed.xml", "/social", "/social.json", "/social.xml", "/icon-192.png", "/icon-512.png", "/icon-maskable-512.png", "/apple-touch-icon.png", "/badge-96.png", "/screenshot-narrow.png", "/screenshot-wide.png":
		return true
	}
	for _, prefix := range []string{"/avatars/", "/scripts/", "/e/", "/a/", "/view/", "/invite/", "/social/", "/chat/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
