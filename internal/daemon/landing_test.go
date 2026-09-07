package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestLandingSignedCreationIsSingleUseAndShowsSource(t *testing.T) {
	app, err := New(context.Background(), Config{DataDir: t.TempDir(), PublicURL: "https://tiny.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	page := httptest.NewRecorder()
	app.ServeLanding(page, httptest.NewRequest(http.MethodGet, "https://tiny.example/relays", nil))
	if !strings.Contains(page.Body.String(), "Source relay") {
		t.Fatal("source field missing")
	}
	if !strings.Contains(page.Body.String(), `class="shell"`) || !strings.Contains(page.Body.String(), "width:360px") || !strings.Contains(page.Body.String(), "min-width:979px") {
		t.Fatal("landing page does not use the fixed 360/960 shell")
	}
	if !strings.Contains(page.Body.String(), `value="search"`) || !strings.Contains(page.Body.String(), `value="home"`) {
		t.Fatal("complete template catalog missing")
	}
	secret, err := event.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	app.cfg.ProvisionOwners = []string{owner}
	body := []byte(`{"name":"owned","template":"default","owner":"` + owner + `","source":"wss://source.example"}`)
	token := landingToken(t, secret, owner, body)
	request := httptest.NewRequest(http.MethodPost, "https://tiny.example/relays", bytes.NewReader(body))
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.ServeLanding(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", response.Code, response.Body.String())
	}
	replay := httptest.NewRequest(http.MethodPost, "https://tiny.example/relays", bytes.NewReader(body))
	replay.Header.Set("Authorization", token)
	replay.Header.Set("Content-Type", "application/json")
	replayResponse := httptest.NewRecorder()
	app.ServeLanding(replayResponse, replay)
	if replayResponse.Code != http.StatusUnauthorized {
		t.Fatalf("replay status %d", replayResponse.Code)
	}
}

func landingToken(t *testing.T, secret, owner string, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	e := event.Event{PubKey: owner, CreatedAt: time.Now().Unix(), Kind: 27235, Tags: [][]string{{"u", "https://tiny.example/relays"}, {"method", "POST"}, {"payload", hex.EncodeToString(sum[:])}}}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return "Nostr " + base64.RawStdEncoding.EncodeToString(raw)
}
