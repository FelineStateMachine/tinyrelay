package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/tinyclient"
)

func TestStandaloneTinyclientAuthenticatesAgainstRealRelay(t *testing.T) {
	_, tenant := testTenant(t)
	upstream := httptest.NewServer(tenant)
	defer upstream.Close()
	handler, err := tinyclient.NewRemote(tinyclient.RemoteOptions{BackendURL: upstream.URL, PublicURL: "http://relay.test"})
	if err != nil {
		t.Fatal(err)
	}
	frontend := httptest.NewServer(handler)
	defer frontend.Close()
	frontendURL, _ := url.Parse(frontend.URL)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	issue := func(method, path, body string, cookie *http.Cookie, signed bool) (*http.Response, string) {
		t.Helper()
		request := httptest.NewRequest(method, "http://relay.test"+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://relay.test")
		if cookie != nil {
			request.AddCookie(cookie)
		}
		if signed {
			signRequest(t, request, body)
		}
		request.URL.Scheme, request.URL.Host = frontendURL.Scheme, frontendURL.Host
		request.RequestURI = ""
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return response, string(raw)
	}
	login, body := issue("POST", "/session", "", nil, true)
	if login.StatusCode != 200 || len(login.Cookies()) != 1 {
		t.Fatalf("signed login: %d %s", login.StatusCode, body)
	}
	cookie := login.Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unsafe cookie: %+v", cookie)
	}
	for _, path := range []string{"/", "/account", "/manage/rules", "/chat", "/scripts/bridge.js", "/manifest.webmanifest"} {
		response, body := issue("GET", path, "", cookie, false)
		integrated := httptest.NewRecorder()
		request := httptest.NewRequest("GET", "http://relay.test"+path, nil)
		request.AddCookie(cookie)
		tenant.ServeHTTP(integrated, request)
		if response.StatusCode != integrated.Code || body != integrated.Body.String() {
			t.Fatalf("standalone parity %s: status=%d want=%d body lengths=%d/%d", path, response.StatusCode, integrated.Code, len(body), integrated.Body.Len())
		}
	}
	denied, _ := issue("POST", "/manage/rpc", `{"method":"stats"}`, cookie, false)
	if denied.StatusCode != 401 {
		t.Fatalf("cookie-only write accepted: %d", denied.StatusCode)
	}
	signed, body := issue("POST", "/manage/rpc", `{"method":"stats"}`, cookie, true)
	if signed.StatusCode != 200 {
		t.Fatalf("signed management request lost URL binding: %d %s", signed.StatusCode, body)
	}
	// The new read bridge ignores claimed pubkeys and still requires a real session.
	forged, body := issue("GET", tinyclient.BackendPath+"?actor="+tenant.Policy().Owner, "", nil, false)
	var snapshot struct {
		Actor string `json:"actor"`
	}
	if forged.StatusCode != 200 || json.Unmarshal([]byte(body), &snapshot) != nil || snapshot.Actor != "" {
		t.Fatalf("forged actor accepted: %d %s", forged.StatusCode, body)
	}
	logout, body := issue("POST", "/session/logout", "", cookie, false)
	if logout.StatusCode != 200 {
		t.Fatalf("logout: %d %s", logout.StatusCode, body)
	}
	response, body := issue("GET", "/account", "", cookie, false)
	if response.StatusCode != 200 || !strings.Contains(body, "Sign in to open your account.") {
		t.Fatalf("revoked session survived: %d", response.StatusCode)
	}
	p := tenant.Policy()
	p.Features.Grasp, p.Features.Grasp08, p.Reads, p.Name = true, true, "members", "private secret project"
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", tinyclient.BackendPath} {
		response, body := issue("GET", path, "", cookie, false)
		if response.StatusCode != 200 || strings.Contains(body, p.Name) || strings.Contains(body, p.Owner) {
			t.Fatalf("private metadata leaked through %s: %d", path, response.StatusCode)
		}
	}
	response, _ = issue("GET", "/social", "", cookie, false)
	if response.StatusCode != 401 {
		t.Fatalf("private read without membership: %d", response.StatusCode)
	}
}
