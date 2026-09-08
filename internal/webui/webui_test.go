package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

type fakeBackend struct {
	policy policy.Policy
	call   string
	params []json.RawMessage
}

type privateFakeBackend struct {
	*fakeBackend
	allowed map[string]bool
}

func (f *privateFakeBackend) ReadAllowed(_ context.Context, actor string) error {
	if f.allowed[actor] {
		return nil
	}
	return context.Canceled
}

func (f *fakeBackend) Query(_ context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	f.call = method + ":" + actor
	f.params = params
	return map[string]any{"method": method}, nil
}
func (f *fakeBackend) Policy() policy.Policy { return f.policy }
func (f *fakeBackend) URL() string           { return "http://relay.example" }
func (f *fakeBackend) Slug() string          { return "demo" }
func (f *fakeBackend) Identity() string      { return strings.Repeat("a", 64) }

func TestPagesUsePlainHTMLAndExposeAllManagementTabs(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/inbox", "/outbox", "/manage/people", "/manage/moderation", "/manage/rules", "/manage/identity", "/manage/connect", "/manage/data", "/manage/sync", "/manage/views", "/manage/health", "/manage/owner"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "<html") || !strings.Contains(recorder.Body.String(), `id="layout"`) {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "fonts.googleapis") || strings.Contains(recorder.Body.String(), "bind.ws") {
			t.Fatalf("%s carries old branding or external style", path)
		}
		if strings.Contains(recorder.Body.String(), "display:flex") || strings.Contains(recorder.Body.String(), "display:grid") {
			t.Fatalf("%s uses a non-table layout", path)
		}
	}
}

func TestPrivatePagesUseGenericShellUntilMembershipIsAuthorized(t *testing.T) {
	p := policy.Defaults(strings.Repeat("a", 64))
	p.Features.Grasp = true
	p.Features.Grasp08 = true
	p.Reads = "members"
	p.Name = "secret tenant"
	p.Description = "secret description"
	backend := &privateFakeBackend{fakeBackend: &fakeBackend{policy: p}, allowed: map[string]bool{p.Owner: true, "member": true}}
	actor := func(r *http.Request) (string, error) {
		if r.Header.Get("X-Actor") == "invalid" {
			return "", context.Canceled
		}
		return r.Header.Get("X-Actor"), nil
	}
	app, err := New(backend, Options{Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		actor string
		code  int
		want  string
		miss  string
	}{
		{name: "anonymous", code: http.StatusOK, want: "Private relay", miss: "secret description"},
		{name: "invalid", actor: "invalid", code: http.StatusOK, want: "Private relay", miss: "secret tenant"},
		{name: "outsider", actor: "outsider", code: http.StatusOK, want: "Private relay", miss: "secret description"},
		{name: "member", actor: "member", code: http.StatusOK, want: "secret tenant", miss: "Private relay"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/manage/identity", nil)
		if tc.actor != "" {
			req.Header.Set("X-Actor", tc.actor)
		}
		res := httptest.NewRecorder()
		app.ServeHTTP(res, req)
		body := res.Body.String()
		if res.Code != tc.code || !strings.Contains(body, tc.want) || strings.Contains(body, tc.miss) {
			t.Fatalf("%s: status=%d want=%q miss=%q body=%s", tc.name, res.Code, tc.want, tc.miss, body)
		}
		if res.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("%s: missing private cache policy", tc.name)
		}
	}
}

func TestPrivateConnectShellKeepsSignerAssetsAvailable(t *testing.T) {
	p := policy.Defaults(strings.Repeat("a", 64))
	p.Features.Grasp = true
	p.Features.Grasp08 = true
	p.Reads = "members"
	p.Name = "secret tenant"
	backend := &privateFakeBackend{fakeBackend: &fakeBackend{policy: p}, allowed: map[string]bool{}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/manage/connect", nil)
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	body := res.Body.String()
	if res.Code != http.StatusOK || !strings.Contains(body, "id=\"session-login\"") || !strings.Contains(body, "/signer.js") || strings.Contains(body, "secret tenant") {
		t.Fatalf("private connect shell invalid: %d %s", res.Code, body)
	}
}

func TestPrivateProtectedEndpointRechecksRevokedMember(t *testing.T) {
	p := policy.Defaults(strings.Repeat("a", 64))
	p.Features.Grasp = true
	p.Features.Grasp08 = true
	p.Reads = "members"
	p.Name = "secret endpoint tenant"
	backend := &privateFakeBackend{fakeBackend: &fakeBackend{policy: p}, allowed: map[string]bool{"member": true}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "member", nil }})
	if err != nil {
		t.Fatal(err)
	}
	backend.allowed["member"] = false
	req := httptest.NewRequest(http.MethodGet, "/card.json", nil)
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden || strings.Contains(res.Body.String(), p.Name) {
		t.Fatalf("revoked member reached protected endpoint: %d %s", res.Code, res.Body.String())
	}
}

func TestDataPageGuidesGitStorageArguments(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/data", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, `method="gitstorage"`) || !strings.Contains(body, `placeholder="64 hex pubkey"`) || !strings.Contains(body, `placeholder="repository name"`) {
		t.Fatalf("gitstorage form is missing guided owner and identifier fields: %s", body)
	}
}

func TestGuidedFormsExposeSourceConsoleControls(t *testing.T) {
	p := policy.Defaults(strings.Repeat("a", 64))
	p.Icon = "https://relay.example/icon.png"
	p.Banner = "https://relay.example/banner.png"
	p.Contact = "owner@example.com"
	p.Tags = []string{"bitcoin", "cats"}
	backend := &fakeBackend{policy: p}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"/manage/people":   {`compose="member"`, `name="member-role"`},
		"/manage/rules":    {`compose="memberInvites"`, `method="setretention"`},
		"/manage/identity": {`method="changerelayicon"`, `compose="identity"`, `Banner URL`},
		"/manage/owner":    {`compose="succession"`, `method="clearsuccession"`},
		"/manage/data":     {`placeholder="64 hex pubkey"`, `placeholder="repository name"`},
	}
	for path, wants := range cases {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		body := recorder.Body.String()
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing guided control %q", path, want)
			}
		}
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/identity", nil))
	body := recorder.Body.String()
	for _, want := range []string{`value="https://relay.example/icon.png"`, `value="owner@example.com"`, `value="bitcoin, cats"`} {
		if !strings.Contains(body, want) {
			t.Errorf("identity form did not preload %q", want)
		}
	}
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/owner", nil))
	if strings.Contains(recorder.Body.String(), `method="deleterelay"><label>Type relay name to confirm <input name="param" value=`) {
		t.Fatal("delete confirmation was prefilled")
	}
}

func TestRPCUsesVerifiedActorAndBackendContract(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "actor", nil }})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/manage/rpc", strings.NewReader(`{"method":"stats","params":[]}`))
	request.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || backend.call != "stats:actor" {
		t.Fatalf("rpc did not reach backend: code=%d call=%q body=%s", recorder.Code, backend.call, recorder.Body.String())
	}
}

func TestSignerBundleAndDedicatedJourneysArePresent(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/manage/connect", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, want := range []string{"bunker://", "nostrconnect", "signer-qr", "27235", "/signer.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("connect page missing %q", want)
		}
	}
	signerRequest := httptest.NewRequest(http.MethodGet, "/signer.js", nil)
	signerResponse := httptest.NewRecorder()
	app.ServeHTTP(signerResponse, signerRequest)
	if signerResponse.Code != http.StatusOK || !strings.Contains(signerResponse.Body.String(), "BunkerSigner") {
		t.Fatalf("signer bundle unavailable: status=%d", signerResponse.Code)
	}
	fixiRequest := httptest.NewRequest(http.MethodGet, "/fixi.js", nil)
	fixiResponse := httptest.NewRecorder()
	app.ServeHTTP(fixiResponse, fixiRequest)
	if fixiResponse.Code != http.StatusOK || !strings.Contains(fixiResponse.Body.String(), "fx-action") {
		t.Fatalf("fixi bundle unavailable: status=%d", fixiResponse.Code)
	}
}

func TestQRAndCardEndpointsProduceNativeArtifacts(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "owner", nil }})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/qr.svg?text=nostrconnect%3A%2F%2Fdemo", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("content-type"), "image/svg+xml") || !strings.Contains(recorder.Body.String(), "shape-rendering") {
		t.Fatalf("invalid QR response: status=%d type=%q", recorder.Code, recorder.Header().Get("content-type"))
	}
	for _, path := range []string{"/card.svg", "/card.nostr"} {
		recorder = httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || (path == "/card.nostr" && !strings.Contains(recorder.Body.String(), "nostr:npub1")) || (path == "/card.svg" && !strings.Contains(recorder.Body.String(), backend.URL())) {
			t.Fatalf("invalid %s response: %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestPublicParityRoutesAndInviteClaim(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	backend.policy.JoinTerms = "Be kind."
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "member", nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/e/" + strings.Repeat("b", 64), "/a/example", "/view/latest", "/invite/code", "/feed.xml"} {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || recorder.Body.Len() == 0 {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	policyRecorder := httptest.NewRecorder()
	app.ServeHTTP(policyRecorder, httptest.NewRequest(http.MethodGet, "/api/join-policy", nil))
	if policyRecorder.Code != http.StatusOK || !strings.Contains(policyRecorder.Body.String(), "Be kind.") {
		t.Fatalf("join policy missing: %d %s", policyRecorder.Code, policyRecorder.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/api/invites/claim", strings.NewReader(`{"method":"claiminvite","params":["code","terms-hash"]}`))
	request.Header.Set("content-type", "application/json")
	app.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || backend.call != "claiminvite:member" || len(backend.params) != 2 {
		t.Fatalf("invite claim did not reach backend: status=%d call=%q body=%s", recorder.Code, backend.call, recorder.Body.String())
	}
}

func TestJobStatusRequiresActorAndEmitsHTMLSSE(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "owner", nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/manage/jobs/status", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("content-type"), "text/event-stream") || !strings.Contains(recorder.Body.String(), "<thead>") || backend.call != "listjobs:owner" {
		t.Fatalf("invalid job stream: status=%d type=%q call=%q body=%s", recorder.Code, recorder.Header().Get("content-type"), backend.call, recorder.Body.String())
	}
}

func TestConnectionFragmentEscapesValues(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "owner", nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/connect/fragment", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("content-type"), "text/html") || !strings.Contains(recorder.Body.String(), "connections-preview") {
		t.Fatalf("invalid connection fragment: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestTenantPrefixRewritesUIEndpoints(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/r/alice/manage/connect", nil))
	body := recorder.Body.String()
	for _, want := range []string{`src="/r/alice/fixi.js"`, `src="/r/alice/signer.js"`, `fx-action="/r/alice/connect/fragment"`, `localPath(this.getAttribute("action")||"/manage/rpc")`, `signedSession("/session")`, `replace(/^http/,"ws")+root`} {
		if !strings.Contains(body, want) {
			t.Errorf("tenant prefix missing %q", want)
		}
	}
}

func TestTenantPrefixHeaderSurvivesDaemonPathStripping(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/manage/connect", nil)
	request.RequestURI = "/r/alice/manage/connect"
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), `fx-action="/r/alice/connect/fragment"`) {
		t.Fatal("daemon prefix header was not applied")
	}
}
