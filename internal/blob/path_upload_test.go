package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPathUploadStoresExactBytesAndReturnsCreatedThenOK(t *testing.T) {
	s := testService(t)
	body := []byte("BUD-13 bytes")
	digest := sha256.Sum256(body)
	sha := hex.EncodeToString(digest[:])
	request := httptest.NewRequest(http.MethodPut, "https://relay.test/"+sha+".txt", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "text/plain")
	first := record(s.Handler(), request)
	if first.Code != http.StatusCreated || !strings.Contains(first.Body.String(), sha) {
		t.Fatalf("first path upload = %d %s", first.Code, first.Body.String())
	}
	second := record(s.Handler(), httptest.NewRequest(http.MethodPut, "https://relay.test/"+sha, strings.NewReader(string(body))))
	if second.Code != http.StatusOK {
		t.Fatalf("duplicate path upload = %d %s", second.Code, second.Body.String())
	}
}

func TestPathUploadRejectsHashMismatchWithoutPersisting(t *testing.T) {
	s := testService(t)
	wanted := strings.Repeat("a", 64)
	response := record(func(w http.ResponseWriter, r *http.Request) { s.pathUpload(w, r, wanted) }, httptest.NewRequest(http.MethodPut, "https://relay.test/"+wanted, strings.NewReader("wrong")))
	if response.Code != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409", response.Code)
	}
	if _, _, err := s.Get(context.Background(), wanted); err == nil {
		t.Fatal("mismatched path upload was persisted")
	}
}

func TestPathUploadAuthenticatesBeforeReadingBody(t *testing.T) {
	s := testService(t)
	read := false
	s.config.Authorize = func(*http.Request, Action) (string, error) { return "", errors.New("bad auth") }
	body := &trackingReader{Reader: strings.NewReader("should not be read"), read: &read}
	sha := strings.Repeat("b", 64)
	response := record(func(w http.ResponseWriter, r *http.Request) { s.pathUpload(w, r, sha) }, httptest.NewRequest(http.MethodPut, "https://relay.test/"+sha, body))
	if response.Code != http.StatusUnauthorized || read {
		t.Fatalf("auth status/read = %d/%v", response.Code, read)
	}
}

func TestPathUploadRejectsRemoteBodyAndMalformedURL(t *testing.T) {
	s := testService(t)
	sha := strings.Repeat("c", 64)
	request := httptest.NewRequest(http.MethodPut, "https://relay.test/"+sha+"?url=https%3A%2F%2Fexample.com%2Fblob", strings.NewReader("body"))
	response := record(func(w http.ResponseWriter, r *http.Request) { s.pathUpload(w, r, sha) }, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("remote body status = %d, want 400", response.Code)
	}
	request = httptest.NewRequest(http.MethodPut, "https://relay.test/"+sha+"?url=file%3A%2F%2F%2Fetc%2Fpasswd", http.NoBody)
	response = record(func(w http.ResponseWriter, r *http.Request) { s.pathUpload(w, r, sha) }, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("malformed URL status = %d, want 400", response.Code)
	}
	request = httptest.NewRequest(http.MethodPut, "https://relay.test/"+sha+"?url=https%3A%2F%2Fexample.com%2Fx%23fragment", http.NoBody)
	response = record(func(w http.ResponseWriter, r *http.Request) { s.pathUpload(w, r, sha) }, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("fragment URL status = %d, want 400", response.Code)
	}
}

func TestPathUploadRemoteSourceVerifiesHashAndSupportsHTTP(t *testing.T) {
	s := testService(t)
	body := []byte("remote BUD-13 bytes")
	digest := sha256.Sum256(body)
	sha := hex.EncodeToString(digest[:])
	s.config.ResolveIP = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
	s.resolve = s.config.ResolveIP
	s.config.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{"Content-Type": []string{"text/plain"}}, Request: req}, nil
	})}
	request := httptest.NewRequest(http.MethodPut, "https://relay.test/"+sha+"?url=http%3A%2F%2Forigin.example%2Ffile.txt", http.NoBody)
	response := record(func(w http.ResponseWriter, r *http.Request) { s.pathUpload(w, r, sha) }, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), sha) {
		t.Fatalf("remote upload = %d %s", response.Code, response.Body.String())
	}
}

func TestPathUploadRemoteRejectsPrivateOrigin(t *testing.T) {
	s := testService(t)
	s.config.ResolveIP = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil }
	s.resolve = s.config.ResolveIP
	response := record(func(w http.ResponseWriter, r *http.Request) { s.pathUpload(w, r, strings.Repeat("d", 64)) }, httptest.NewRequest(http.MethodPut, "https://relay.test/"+strings.Repeat("d", 64)+"?url=http%3A%2F%2Forigin.example%2Fx", http.NoBody))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("private origin status = %d, want 400", response.Code)
	}
}

func TestPathUploadRemoteUsesRequestContext(t *testing.T) {
	s := testService(t)
	sha := strings.Repeat("e", 64)
	s.config.ResolveIP = func(ctx context.Context, _ string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	s.resolve = s.config.ResolveIP
	s.config.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPut, "https://relay.test/"+sha+"?url=https%3A%2F%2Forigin.example%2Ffile", http.NoBody).WithContext(ctx)
	response := record(s.Handler(), request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("cancelled remote upload = %d, want 502", response.Code)
	}
}

type trackingReader struct {
	io.Reader
	read *bool
}

func (r *trackingReader) Read(p []byte) (int, error) {
	*r.read = true
	return r.Reader.Read(p)
}

func TestPathUploadPreservesAccessPurposeAndClaimMetadata(t *testing.T) {
	s := testService(t)
	body := "named member-only file"
	digest := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(digest[:])
	r := httptest.NewRequest(http.MethodPut, "https://relay.test/"+hash+"?filename=note.txt&path=docs%2Fnote.txt&access=members&purpose=file", strings.NewReader(body))
	r.Header.Set("Content-Type", "text/plain")
	response := record(s.Handler(), r)
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", response.Code, response.Body.String())
	}
	entries, err := s.ListClaimMetadata(t.Context(), "uploader")
	if err != nil || len(entries) != 1 || entries[0].Purpose != "file" || entries[0].Name != "note.txt" || entries[0].Path != "docs/note.txt" {
		t.Fatalf("claim: %+v %v", entries, err)
	}
	if access, err := s.BlobAccess(t.Context(), hash); err != nil || access != AccessMembers {
		t.Fatalf("access: %q %v", access, err)
	}
}
