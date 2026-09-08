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
)

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
