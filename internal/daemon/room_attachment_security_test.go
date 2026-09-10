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

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func roomAttachmentAuth(t *testing.T, secret, rawURL, method, body string) string {
	t.Helper()
	hash := sha256.Sum256([]byte(body))
	e := event.Event{Kind: 27235, CreatedAt: time.Now().Unix(), Tags: [][]string{{"u", rawURL}, {"method", method}, {"payload", hex.EncodeToString(hash[:])}}}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return "Nostr " + base64.RawURLEncoding.EncodeToString(raw)
}

func roomAttachmentBlossomAuth(t *testing.T, secret, action, hash string) string {
	t.Helper()
	e := event.Event{Kind: 24242, CreatedAt: time.Now().Unix(), Tags: [][]string{{"t", action}, {"x", hash}, {"expiration", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)}}}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	// The daemon's authorization parser uses the Nostr scheme for both NIP-98
	// and kind-24242 Blossom proofs; the event kind selects the verifier.
	return "Nostr " + base64.RawURLEncoding.EncodeToString(raw)
}

func TestRoomAttachmentUploadAndPrivateReadDoors(t *testing.T) {
	h := newRoomHarness(t)
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "secret"}, {"closed"}}, "")
	body := "private attachment"
	path := "/rooms/secret/attachments?filename=secret.txt"
	req := httptest.NewRequest(http.MethodPut, "http://relay.test"+path, strings.NewReader(body))
	req.Header.Set("Authorization", roomAttachmentAuth(t, h.secrets["alice"], req.URL.String(), req.Method, body))
	req.Header.Set("Content-Type", "text/plain")
	res := httptest.NewRecorder()
	h.tenant.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("member upload = %d: %s", res.Code, res.Body.String())
	}
	var uploaded struct {
		Hash string `json:"sha256"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &uploaded); err != nil || len(uploaded.Hash) != 64 {
		t.Fatalf("upload response = %s: %v", res.Body.String(), err)
	}

	for _, door := range []string{"/media/" + uploaded.Hash, "/files/raw?hash=" + uploaded.Hash} {
		t.Run("outsider "+door, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://relay.test"+door, nil)
			req.Header.Set("Authorization", roomAttachmentAuth(t, h.secrets["bob"], req.URL.String(), req.Method, ""))
			got := httptest.NewRecorder()
			h.tenant.ServeHTTP(got, req)
			if got.Code != http.StatusForbidden && got.Code != http.StatusUnauthorized {
				t.Fatalf("outsider read = %d: %s", got.Code, got.Body.String())
			}
		})
		t.Run("member "+door, func(t *testing.T) {
			// Alice owns the closed room; a signed GET must be accepted.
			req := httptest.NewRequest(http.MethodGet, "http://relay.test"+door, nil)
			req.Header.Set("Authorization", roomAttachmentAuth(t, h.secrets["alice"], req.URL.String(), req.Method, ""))
			got := httptest.NewRecorder()
			h.tenant.ServeHTTP(got, req)
			if got.Code != http.StatusOK || got.Body.String() != body {
				t.Fatalf("member read = %d %q", got.Code, got.Body.String())
			}
		})
	}
	for _, door := range []string{"/" + uploaded.Hash + ".txt", "/media/" + uploaded.Hash + ".txt"} {
		t.Run("blossom outsider "+door, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://relay.test"+door, nil)
			req.Header.Set("Authorization", roomAttachmentBlossomAuth(t, h.secrets["bob"], "get", uploaded.Hash))
			got := httptest.NewRecorder()
			h.tenant.ServeHTTP(got, req)
			if got.Code != http.StatusForbidden && got.Code != http.StatusUnauthorized {
				t.Fatalf("Blossom outsider read = %d: %s", got.Code, got.Body.String())
			}
		})
	}
	for _, door := range []string{"/" + uploaded.Hash + ".txt", "/media/" + uploaded.Hash + ".txt"} {
		req := httptest.NewRequest(http.MethodGet, "http://relay.test"+door, nil)
		req.Header.Set("Authorization", roomAttachmentBlossomAuth(t, h.secrets["alice"], "get", uploaded.Hash))
		got := httptest.NewRecorder()
		h.tenant.ServeHTTP(got, req)
		if got.Code != http.StatusOK || got.Body.String() != body {
			t.Fatalf("Blossom member read = %d %q", got.Code, got.Body.String())
		}
	}
	if _, err := h.tenant.Execute(h.ctx, h.keys["bob"], "browsefile", []json.RawMessage{rawJSON(map[string]any{"hash": uploaded.Hash})}); err == nil {
		t.Fatal("outsider browsefile exposed private attachment")
	}
}

func TestRoomAttachmentMediaSandboxesActiveTypes(t *testing.T) {
	h := newRoomHarness(t)
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "secret"}, {"closed"}}, "")
	body := "<html><script>document.body.dataset.pwned='yes'</script></html>"
	req := httptest.NewRequest(http.MethodPut, "http://relay.test/rooms/secret/attachments?filename=x.html", strings.NewReader(body))
	req.Header.Set("Authorization", roomAttachmentAuth(t, h.secrets["alice"], req.URL.String(), req.Method, body))
	req.Header.Set("Content-Type", "text/html")
	res := httptest.NewRecorder()
	h.tenant.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("upload = %d: %s", res.Code, res.Body.String())
	}
	var uploaded struct {
		Hash string `json:"sha256"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &uploaded)
	get := httptest.NewRequest(http.MethodGet, "http://relay.test/media/"+uploaded.Hash, nil)
	get.Header.Set("Authorization", roomAttachmentAuth(t, h.secrets["alice"], get.URL.String(), get.Method, ""))
	got := httptest.NewRecorder()
	h.tenant.ServeHTTP(got, get)
	if got.Code != http.StatusOK {
		t.Fatalf("media read = %d: %s", got.Code, got.Body.String())
	}
	if got.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(got.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("active media lacks isolation: nosniff=%q csp=%q", got.Header().Get("X-Content-Type-Options"), got.Header().Get("Content-Security-Policy"))
	}
	if got.Header().Get("Content-Type") == "text/html" {
		t.Fatal("HTML attachment served as active text/html")
	}
}

func TestRoomAttachmentScopeDiesWithMembershipAndRoomGeneration(t *testing.T) {
	h := newRoomHarness(t)
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "removed"}, {"closed"}}, "")
	h.must("alice", event.KIND_PUT_USER, [][]string{{"h", "removed"}, {"p", h.keys["bob"]}}, "")
	removed, err := h.tenant.storeRoomAttachment(h.ctx, h.keys["bob"], "removed", roomAttachmentInput{Body: []byte("member bytes"), Type: "text/plain"})
	if err != nil {
		t.Fatalf("removed-member attachment = %v", err)
	}
	h.must("alice", event.KIND_REMOVE_USER, [][]string{{"h", "removed"}, {"p", h.keys["bob"]}}, "")
	if err := h.tenant.roomAttachmentAccess(h.ctx, h.keys["bob"], removed["sha256"].(string)); err == nil {
		t.Fatal("removed member retained attachment access")
	}

	created := h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "reused"}, {"closed"}}, "")
	body := []byte("old room bytes")
	result, err := h.tenant.storeRoomAttachment(h.ctx, h.keys["alice"], "reused", roomAttachmentInput{Body: body, Type: "text/plain", Filename: "old.txt"})
	if err != nil {
		t.Fatalf("store attachment = %v", err)
	}
	hash := result["sha256"].(string)
	if err := h.tenant.roomAttachmentAccess(h.ctx, h.keys["alice"], hash); err != nil {
		t.Fatalf("initial room access = %v", err)
	}
	// Deleting the room must invalidate its attachment scope, even if the
	// identifier is later reused for a newly created room.
	h.must("alice", event.KIND_DELETE_GROUP, [][]string{{"h", "reused"}}, "")
	h.projectRooms()
	if err := h.tenant.roomAttachmentAccess(h.ctx, h.keys["alice"], hash); err == nil {
		t.Fatal("deleted room retained attachment access")
	}
	newRoom := h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "reused"}, {"closed"}, {"name", "Reused again"}}, "")
	if created.ID == newRoom.ID {
		t.Fatal("room recreation did not produce a new event")
	}
	if err := h.tenant.roomAttachmentAccess(h.ctx, h.keys["alice"], hash); err == nil {
		t.Fatal("new room generation inherited old attachment")
	}
}

func TestRoomAttachmentHTTPRejectsChangedPayload(t *testing.T) {
	h := newRoomHarness(t)
	request := httptest.NewRequest(http.MethodPut, "http://relay.test/rooms/main/attachments", strings.NewReader("changed bytes"))
	request.Header.Set("Authorization", roomAttachmentAuth(t, h.secrets["owner"], request.URL.String(), request.Method, "signed bytes"))
	request.Header.Set("Content-Type", "text/plain")
	response := httptest.NewRecorder()
	h.tenant.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("changed payload: %d %s", response.Code, response.Body.String())
	}
	var count int
	if err := h.tenant.store.DB().QueryRowContext(h.ctx, "SELECT COUNT(*) FROM room_attachments").Scan(&count); err != nil || count != 0 {
		t.Fatalf("changed payload was stored: %d %v", count, err)
	}
}
