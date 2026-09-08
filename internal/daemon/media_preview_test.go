package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
)

// pngHeader is the start of a valid PNG file: enough for type detection.
var pngHeader = append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, make([]byte, 64)...)

func TestUntypedImageUploadsAreRecognisedAndServedInline(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	entry, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader(string(pngHeader)), Type: "application/octet-stream", Uploader: tenant.Policy().Owner})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Type != "image/png" {
		t.Fatalf("stored type = %q, want image/png", entry.Type)
	}
	value, err := tenant.Execute(ctx, tenant.Policy().Owner, "browsefile", []json.RawMessage{json.RawMessage(`{"hash":"` + entry.SHA256 + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if page, _ := value.(map[string]any); page["type"] != "image/png" || page["binary"] != true {
		t.Fatalf("browsefile = %v", value)
	}
	req := httptest.NewRequest(http.MethodGet, "http://relay.test/files/raw?hash="+entry.SHA256, nil)
	res := httptest.NewRecorder()
	tenant.ServeHTTP(res, req)
	if res.Code != 200 || res.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(res.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("image download: %d type=%q disposition=%q", res.Code, res.Header().Get("Content-Type"), res.Header().Get("Content-Disposition"))
	}
	// Text that merely claims to be an image is still not rendered as a page.
	html, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("<html>hi</html>"), Type: "application/octet-stream", Uploader: tenant.Policy().Owner})
	if err != nil {
		t.Fatal(err)
	}
	res = httptest.NewRecorder()
	tenant.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://relay.test/files/raw?hash="+html.SHA256, nil))
	if res.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(res.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("html blob served as %q %q", res.Header().Get("Content-Type"), res.Header().Get("Content-Disposition"))
	}
}
