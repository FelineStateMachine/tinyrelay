package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
)

func TestEncryptedFileMetadataEndpointBindsBodyAndClaim(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := t.Context()
	entry, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("ciphertext"), Type: "application/octet-stream", Uploader: tenant.Policy().Owner})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"hash":"` + entry.SHA256 + `","name":"Encrypted file ` + entry.SHA256[:12] + `","path":"Encrypted file ` + entry.SHA256[:12] + `","purpose":"file","size":104857600,"type":"application/octet-stream"}`
	r := httptest.NewRequest(http.MethodPost, "http://relay.test/files/metadata", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	signRequest(t, r, body)
	w := httptest.NewRecorder()
	tenant.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("metadata status=%d body=%s", w.Code, w.Body.String())
	}
	var purpose, typ string
	var size int64
	if err := tenant.store.DB().QueryRow("SELECT purpose,logical_size,logical_type FROM blob_claim_metadata WHERE sha256=? AND uploader=?", entry.SHA256, tenant.Policy().Owner).Scan(&purpose, &size, &typ); err != nil {
		t.Fatal(err)
	}
	if purpose != "file" || size != 104857600 || typ != "application/octet-stream" {
		t.Fatalf("metadata=%q %d %q", purpose, size, typ)
	}
	bad := strings.Replace(body, `"size":104857600`, `"size":104857601`, 1)
	r = httptest.NewRequest(http.MethodPost, "http://relay.test/files/metadata", strings.NewReader(bad))
	r.Header.Set("Content-Type", "application/json")
	signRequest(t, r, body)
	w = httptest.NewRecorder()
	tenant.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered metadata status=%d body=%s", w.Code, w.Body.String())
	}
	unknown := body[:len(body)-1] + `,"key":"must-not-store"}`
	r = httptest.NewRequest(http.MethodPost, "http://relay.test/files/metadata", strings.NewReader(unknown))
	r.Header.Set("Content-Type", "application/json")
	signRequest(t, r, unknown)
	w = httptest.NewRecorder()
	tenant.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown metadata status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestEncryptedUploadFinalizesOneLogicalLibraryFile(t *testing.T) {
	_, tenant := testTenant(t)
	body := "an encrypted upload fixture"
	r := httptest.NewRequest(http.MethodPut, "http://relay.test/upload?purpose=chunk", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/octet-stream")
	signRequest(t, r, body)
	w := httptest.NewRecorder()
	tenant.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var uploaded struct {
		Hash string `json:"sha256"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	query := []json.RawMessage{json.RawMessage(`{"view":"library"}`)}
	library, err := tenant.Execute(t.Context(), tenant.Policy().Owner, "browsefiles", query)
	if err != nil {
		t.Fatal(err)
	}
	if len(fileItems(t, library)) != 0 {
		t.Fatalf("internal chunk in library: %v", library)
	}
	metadata := `{"hash":"` + uploaded.Hash + `","name":"Encrypted file","purpose":"file","size":104857600,"type":"application/octet-stream"}`
	r = httptest.NewRequest(http.MethodPost, "http://relay.test/files/metadata", strings.NewReader(metadata))
	signRequest(t, r, metadata)
	w = httptest.NewRecorder()
	tenant.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("finalize: %d %s", w.Code, w.Body.String())
	}
	library, err = tenant.Execute(t.Context(), tenant.Policy().Owner, "browsefiles", query)
	if err != nil {
		t.Fatal(err)
	}
	items := fileItems(t, library)
	if len(items) != 1 || items[0]["name"] != "Encrypted file" || items[0]["size"] != int64(104857600) {
		t.Fatalf("logical file: %v", items)
	}
}

func TestFileMetadataCannotChangeAnotherUploadersClaim(t *testing.T) {
	_, tenant := testTenant(t)
	entry, err := tenant.blobs.Put(t.Context(), blob.PutOptions{Reader: strings.NewReader("someone else's file"), Uploader: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"hash":"` + entry.SHA256 + `","name":"Changed","purpose":"file"}`
	r := httptest.NewRequest(http.MethodPost, "http://relay.test/files/metadata", strings.NewReader(body))
	signRequest(t, r, body)
	w := httptest.NewRecorder()
	tenant.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatal("owner must not edit another uploader's claim metadata")
	}
	var count int
	if err := tenant.store.DB().QueryRow("SELECT count(*) FROM blob_claim_metadata WHERE sha256=?", entry.SHA256).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("metadata changed without claim ownership")
	}
}
