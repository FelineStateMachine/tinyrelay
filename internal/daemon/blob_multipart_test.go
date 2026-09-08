package daemon

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func multipartAuthorization(t *testing.T, hash string) string {
	t.Helper()
	token := event.Event{Kind: 24242, CreatedAt: time.Now().Unix(), Tags: [][]string{
		{"t", "upload"}, {"x", hash}, {"expiration", strconv.FormatInt(time.Now().Unix()+60, 10)},
	}, Content: "Authorize multipart upload"}
	if err := event.Sign(&token, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	return "Nostr " + base64.RawURLEncoding.EncodeToString(raw)
}

func TestMultipartAuthorizationBindsFinalHashBeforeReading(t *testing.T) {
	_, tenant := testTenant(t)
	finalHash := strings.Repeat("a", 64)
	r := httptest.NewRequest(http.MethodPatch, "http://relay.test/"+finalHash+".txt", strings.NewReader("chunk"))
	r.Header.Set("Authorization", multipartAuthorization(t, strings.Repeat("b", 64)))
	if _, err := tenant.authorizeBlob(r, blob.ActionUpload); err == nil {
		t.Fatal("multipart authorization accepted another final hash")
	}
	r.Header.Set("Authorization", multipartAuthorization(t, finalHash))
	if _, err := tenant.authorizeBlob(r, blob.ActionUpload); err != nil {
		t.Fatal(err)
	}
	chunkHash := sha256.Sum256([]byte("chunk"))
	if err := tenant.validateBlobUpload(r, hex.EncodeToString(chunkHash[:])); err != nil {
		t.Fatalf("Blossom proof incorrectly required a chunk hash: %v", err)
	}
}

func TestMultipartNIP98BindsEachChunkBody(t *testing.T) {
	_, tenant := testTenant(t)
	r := httptest.NewRequest(http.MethodPatch, "http://relay.test/"+strings.Repeat("c", 64), strings.NewReader("chunk"))
	signRequest(t, r, "chunk")
	if _, err := tenant.authorizeBlob(r, blob.ActionUpload); err != nil {
		t.Fatal(err)
	}
	chunkHash := sha256.Sum256([]byte("chunk"))
	if err := tenant.validateBlobUpload(r, hex.EncodeToString(chunkHash[:])); err != nil {
		t.Fatal(err)
	}
	if err := tenant.validateBlobUpload(r, strings.Repeat("c", 64)); err == nil {
		t.Fatal("NIP-98 proof accepted final hash in place of chunk payload")
	}
}

func TestMultipartPreflightAdvertisesMountedFileDoor(t *testing.T) {
	_, tenant := testTenant(t)
	r := httptest.NewRequest(http.MethodOptions, "http://relay.test/"+strings.Repeat("a", 64), nil)
	w := httptest.NewRecorder()
	tenant.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || !strings.Contains(w.Header().Get("Allow"), "PATCH") {
		t.Fatalf("multipart preflight: %d, Allow=%q", w.Code, w.Header().Get("Allow"))
	}
	for _, header := range []string{"upload-type", "upload-length", "upload-offset"} {
		if !strings.Contains(strings.ToLower(w.Header().Get("Access-Control-Allow-Headers")), header) {
			t.Fatalf("multipart CORS does not allow %s", header)
		}
	}
	if !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "Allow") {
		t.Fatal("browser cannot read PATCH support")
	}
	for _, capability := range tenant.Capabilities(nil) {
		if capability.ID == "BUD-14" && capability.Status == "enabled" {
			return
		}
	}
	t.Fatal("BUD-14 capability is missing")
}

func TestMultipartHTTPVerifiesEachNIP98ChunkBeforeAcceptingIt(t *testing.T) {
	_, tenant := testTenant(t)
	const body = "first and second"
	digest := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(digest[:])
	patch := func(offset int, chunk, signed string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "http://relay.test/"+hash, strings.NewReader(chunk))
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("Upload-Type", "text/plain")
		r.Header.Set("Upload-Length", strconv.Itoa(len(body)))
		r.Header.Set("Upload-Offset", strconv.Itoa(offset))
		signRequest(t, r, signed)
		w := httptest.NewRecorder()
		tenant.ServeHTTP(w, r)
		return w
	}
	if w := patch(0, body[:6], "altered"); w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid chunk proof = %d: %s", w.Code, w.Body.String())
	}
	if w := patch(6, body[6:], body[6:]); w.Code != http.StatusNoContent {
		t.Fatalf("out-of-order chunk = %d: %s", w.Code, w.Body.String())
	}
	if w := patch(0, body[:6], body[:6]); w.Code != http.StatusCreated {
		t.Fatalf("complete upload = %d: %s", w.Code, w.Body.String())
	}
	if w := patch(0, body[:6], "still altered"); w.Code != http.StatusUnauthorized {
		t.Fatalf("existing blob bypassed payload validation = %d: %s", w.Code, w.Body.String())
	}
	get := httptest.NewRecorder()
	tenant.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "http://relay.test/"+hash, nil))
	if get.Code != http.StatusOK || get.Body.String() != body {
		t.Fatalf("retrieved upload = %d: %s", get.Code, get.Body.String())
	}
}
