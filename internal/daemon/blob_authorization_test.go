package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
)

// agentUpload sends one Blossom PUT /upload signed by secret through the
// tenant and returns the response.
func agentUpload(t *testing.T, tenant *Tenant, secret, body, typ string) *httptest.ResponseRecorder {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	token := event.Event{Kind: 24242, CreatedAt: time.Now().Unix(), Tags: [][]string{{"t", "upload"}, {"x", hex.EncodeToString(sum[:])}, {"expiration", strconv.FormatInt(time.Now().Unix()+60, 10)}}, Content: "Authorize file storage"}
	if err := event.Sign(&token, secret); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "http://relay.test/upload", strings.NewReader(body))
	r.Header.Set("Authorization", "Nostr "+base64.RawURLEncoding.EncodeToString(raw))
	r.Header.Set("Content-Type", typ)
	w := httptest.NewRecorder()
	tenant.ServeHTTP(w, r)
	return w
}

// TestAgentGrantGovernsUploads covers the blob door for agent keys: an
// active grant uploads as a member, a paused or revoked one is refused, a
// sites ttl stamps the claim and the maintenance sweep retires it, and an
// encrypted grant refuses plain files.
func TestAgentGrantGovernsUploads(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	agent, _ := event.PublicKey(testAgentSecret)
	if _, err := tenant.Execute(ctx, owner, "setpolicy", []json.RawMessage{json.RawMessage(`{"writes":"allowlist"}`)}); err != nil {
		t.Fatal(err)
	}
	if w := agentUpload(t, tenant, testAgentSecret, "stranger", "text/plain"); w.Code == http.StatusOK {
		t.Fatal("a key without a grant uploaded under allowlist writes")
	}
	now := time.Now().Unix()
	own := sites.SiteLabel(event.Event{Kind: sites.KindSite, PubKey: agent})
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now, now+3600, []string{"sites", own, "ttl=2"})); err != nil {
		t.Fatal(err)
	}
	if w := agentUpload(t, tenant, testAgentSecret, "site file", "text/html"); w.Code != http.StatusOK {
		t.Fatalf("granted agent upload = %d %s", w.Code, w.Body.String())
	}
	sum := sha256.Sum256([]byte("site file"))
	hash := hex.EncodeToString(sum[:])
	var expires int64
	if err := tenant.store.DB().QueryRow("SELECT expires FROM blob_claims WHERE sha256=? AND uploader=?", hash, agent).Scan(&expires); err != nil || expires < now+2*86400 || expires > now+2*86400+60 {
		t.Fatalf("claim expires=%d err=%v, want about %d", expires, err, now+2*86400)
	}
	if entries, err := tenant.blobs.ListMetadata(ctx, agent); err != nil || len(entries) != 1 {
		t.Fatalf("agent list = %d entries, err=%v", len(entries), err)
	}
	if _, err := tenant.Execute(ctx, owner, "pauseagent", []json.RawMessage{json.RawMessage(strconv.Quote(agent))}); err != nil {
		t.Fatal(err)
	}
	if w := agentUpload(t, tenant, testAgentSecret, "while paused", "text/html"); w.Code == http.StatusOK || !strings.Contains(w.Body.String(), "paused") {
		t.Fatalf("paused agent upload = %d %s", w.Code, w.Body.String())
	}
	if _, err := tenant.Execute(ctx, owner, "resumeagent", []json.RawMessage{json.RawMessage(strconv.Quote(agent))}); err != nil {
		t.Fatal(err)
	}
	if w := agentUpload(t, tenant, testAgentSecret, "after resume", "text/html"); w.Code != http.StatusOK {
		t.Fatalf("resumed agent upload = %d %s", w.Code, w.Body.String())
	}
	if _, err := tenant.Execute(ctx, owner, "revokeagent", []json.RawMessage{json.RawMessage(strconv.Quote(agent))}); err != nil {
		t.Fatal(err)
	}
	if w := agentUpload(t, tenant, testAgentSecret, "after revoke", "text/html"); w.Code == http.StatusOK {
		t.Fatalf("revoked agent upload = %d %s", w.Code, w.Body.String())
	}
	// The sweep removes the agent's files once the ttl lapses.
	if err := tenant.sweepExpiredBlobs(ctx, now+2*86400+61); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tenant.blobs.Get(ctx, hash); err == nil {
		t.Fatal("expired agent upload still stored")
	}
	// A fresh grant with the encrypted flag refuses plain uploads.
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now+1, now+3600, []string{"sites", "*", "ttl=1", "encrypted"})); err != nil {
		t.Fatal(err)
	}
	if w := agentUpload(t, tenant, testAgentSecret, "<html>plain</html>", "text/html"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "requires encrypted uploads") {
		t.Fatalf("plain upload under encrypted grant = %d %s", w.Code, w.Body.String())
	}
	if w := agentUpload(t, tenant, testAgentSecret, "\x8f\x1a\x9c\x03"+strings.Repeat("\xe2\x71\x05\x9b\x4c", 20), "application/octet-stream"); w.Code != http.StatusOK {
		t.Fatalf("ciphertext upload under encrypted grant = %d %s", w.Code, w.Body.String())
	}
}

type blobOriginTransport func(*http.Request) (*http.Response, error)

func (f blobOriginTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBlobMirrorAuthenticationBindsRequestAndFetchedContent(t *testing.T) {
	const content = "fetched encrypted bytes"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	for _, tc := range []struct {
		name, mode, action, scope string
		pathUpload, tamper        bool
		want                      int
	}{
		{name: "mirror NIP98", mode: "nip98", want: 201},
		{name: "mirror NIP98 tampered JSON", mode: "nip98", tamper: true, want: 401},
		{name: "mirror Blossom upload verb", action: "upload", scope: hash, want: 201},
		{name: "mirror Blossom wrong verb", action: "mirror", scope: hash, want: 401},
		{name: "mirror Blossom wrong content", action: "upload", scope: strings.Repeat("a", 64), want: 401},
		{name: "remote NIP98", mode: "nip98", pathUpload: true, want: 201},
		{name: "remote Blossom", action: "upload", scope: hash, pathUpload: true, want: 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tenant := testTenant(t)
			fetches := 0
			service, err := blob.New(context.Background(), blob.Config{
				Root: t.TempDir(), Store: tenant.store, PublicURL: tenant.publicURL,
				Authorize: tenant.authorizeBlob, ValidateUpload: tenant.validateBlobUpload,
				ResolveIP: func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil },
				HTTPClient: &http.Client{Transport: blobOriginTransport(func(*http.Request) (*http.Response, error) {
					fetches++
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader(content))}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			body := `{"url":"https://files.example/source"}`
			target := "http://relay.test/mirror"
			if tc.pathUpload {
				body = ""
				target = "http://relay.test/" + hash + "?url=" + url.QueryEscape("https://files.example/source")
			}
			r := httptest.NewRequest(http.MethodPut, target, strings.NewReader(body))
			if tc.mode == "nip98" {
				signRequest(t, r, body)
			} else {
				token := event.Event{Kind: 24242, CreatedAt: time.Now().Unix(), Tags: [][]string{{"t", tc.action}, {"x", tc.scope}, {"expiration", strconv.FormatInt(time.Now().Unix()+60, 10)}}, Content: "Authorize file storage"}
				if err := event.Sign(&token, strings.Repeat("0", 63)+"1"); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(token)
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Authorization", "Nostr "+base64.RawURLEncoding.EncodeToString(raw))
			}
			if tc.tamper {
				r.Body = io.NopCloser(strings.NewReader(`{"url":"https://files.example/altered"}`))
			}
			w := httptest.NewRecorder()
			service.Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if (tc.tamper || tc.action == "mirror") && fetches != 0 {
				t.Fatal("unauthorized request fetched an origin")
			}
		})
	}
}

func TestUploadURLQueryCannotBypassNIP98PayloadValidation(t *testing.T) {
	_, tenant := testTenant(t)
	r := httptest.NewRequest(http.MethodPut, "http://relay.test/upload?url=https://files.example/ignored", strings.NewReader("tampered body"))
	signRequest(t, r, "signed body")
	w := httptest.NewRecorder()
	tenant.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered upload status=%d: %s", w.Code, w.Body.String())
	}
}
