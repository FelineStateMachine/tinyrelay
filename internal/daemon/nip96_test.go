package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestNIP96MultipartUploadGetAndDeleteOnNonDefaultTenant(t *testing.T) {
	ctx := context.Background()
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	meta, err := app.Create(ctx, CreateOptions{Name: "media", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="photo.webp"`},
		"Content-Type":        {"image/webp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "webp payload"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(nil), body.Bytes()...)
	url := "http://relay.test/r/media/nip96"
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", form.FormDataContentType())
	signRequest(t, req, string(raw))
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", res.Code, res.Body.String())
	}
	var descriptor struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &descriptor); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(descriptor.URL, "/r/media/") || !strings.HasSuffix(descriptor.URL, ".webp") {
		t.Fatalf("descriptor URL=%q", descriptor.URL)
	}

	sha := strings.TrimSuffix(strings.TrimPrefix(descriptor.URL, "http://relay.test/r/media/"), ".webp")
	getURL := "http://relay.test/r/media/nip96/" + sha + ".webp"
	get := httptest.NewRequest(http.MethodGet, getURL, nil)
	signRequest(t, get, "")
	getRes := httptest.NewRecorder()
	app.ServeHTTP(getRes, get)
	if getRes.Code != http.StatusOK || getRes.Body.String() != "webp payload" {
		t.Fatalf("get url=%s status=%d body=%q", getURL, getRes.Code, getRes.Body.String())
	}

	del := httptest.NewRequest(http.MethodDelete, getURL, nil)
	signRequest(t, del, "")
	delRes := httptest.NewRecorder()
	app.ServeHTTP(delRes, del)
	if delRes.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", delRes.Code, delRes.Body.String())
	}
	tenant := app.tenants[meta.ID]
	updated := tenant.Policy()
	updated.Features.Files = false
	if err := tenant.applyPolicy(ctx, updated); err != nil {
		t.Fatal(err)
	}
	disabled := httptest.NewRequest(http.MethodGet, getURL, nil)
	disabledRes := httptest.NewRecorder()
	app.ServeHTTP(disabledRes, disabled)
	if disabledRes.Code != http.StatusNotFound {
		t.Fatalf("disabled files status=%d", disabledRes.Code)
	}

	bad := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	bad.Header.Set("Content-Type", form.FormDataContentType())
	signRequest(t, bad, "tampered")
	badRes := httptest.NewRecorder()
	app.ServeHTTP(badRes, bad)
	if badRes.Code == http.StatusCreated || badRes.Code == http.StatusOK {
		t.Fatalf("tampered payload accepted: %d %s", badRes.Code, badRes.Body.String())
	}
}
