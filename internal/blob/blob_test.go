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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestUploadDeduplicatesAndServesByHash(t *testing.T) {
	service := testService(t)
	body := []byte("hello blob")
	hash := sha256.Sum256(body)
	digest := hex.EncodeToString(hash[:])
	req := httptest.NewRequest(http.MethodPut, "https://relay.test/upload", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "text/plain")
	first := service.Handler().ServeHTTP
	response := record(first, req)
	if response.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want 200 (%s)", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), digest) {
		t.Fatalf("upload descriptor lacks hash: %s", response.Body.String())
	}
	req = httptest.NewRequest(http.MethodPut, "https://relay.test/upload", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "text/plain")
	response = record(first, req)
	if response.Code != http.StatusOK {
		t.Fatalf("duplicate upload status = %d, want 200", response.Code)
	}
	response = record(first, httptest.NewRequest(http.MethodGet, "https://relay.test/"+digest+".txt", nil))
	if response.Code != http.StatusOK || response.Body.String() != string(body) {
		t.Fatalf("download = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("ETag") != `"`+digest+`"` {
		t.Fatalf("etag = %q", response.Header().Get("ETag"))
	}
}

func TestUploadEnforcesFileAndUploaderLimits(t *testing.T) {
	service := testService(t)
	service.config.Limits = func() Limits { return Limits{MaxFileBytes: 4, UserStorageBytes: 6} }
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("12345"), Type: "text/plain", Uploader: testUploader}); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("oversized upload error = %v", err)
	}
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("1234"), Type: "text/plain", Uploader: testUploader}); err != nil {
		t.Fatalf("first limited upload: %v", err)
	}
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("5678"), Type: "text/plain", Uploader: testUploader}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("quota upload error = %v", err)
	}
	// A deduplicated claim does not consume additional physical storage.
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("1234"), Type: "text/plain", Uploader: strings.Repeat("b", 64)}); err != nil {
		t.Fatalf("deduplicated upload: %v", err)
	}
	second := strings.Repeat("b", 64)
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("abc"), Type: "text/plain", Uploader: strings.Repeat("c", 64)}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("56"), Type: "text/plain", Uploader: second}); err != nil {
		t.Fatalf("second uploader upload: %v", err)
	}
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("xyz"), Type: "text/plain", Uploader: second}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("second uploader remaining quota error = %v", err)
	}
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("abc"), Type: "text/plain", Uploader: second}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("existing blob claim bypassed remaining quota: %v", err)
	}
	digest := sha256.Sum256([]byte("56"))
	sha := hex.EncodeToString(digest[:])
	if err := service.DeleteForUploader(context.Background(), sha, second); err != nil {
		t.Fatalf("remove second uploader claim: %v", err)
	}
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("ab"), Type: "text/plain", Uploader: second}); err != nil {
		t.Fatalf("quota after claim deletion: %v", err)
	}
}

func TestConcurrentUploadsRespectUploaderQuota(t *testing.T) {
	service := testService(t)
	service.config.Limits = func() Limits { return Limits{UserStorageBytes: 4} }
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, body := range []string{"aaaa", "bbbb"} {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			_, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader(body), Type: "text/plain", Uploader: testUploader})
			errs <- err
		}(body)
	}
	wg.Wait()
	close(errs)
	var success, quota int
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrQuotaExceeded) {
			quota++
		}
	}
	if success != 1 || quota != 1 {
		t.Fatalf("concurrent quota results success=%d quota=%d", success, quota)
	}
}

func TestQuotaOnlyLimitReportsQuotaBeforeHashMismatch(t *testing.T) {
	service := testService(t)
	service.config.Limits = func() Limits { return Limits{UserStorageBytes: 4} }
	_, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("12345"), Type: "text/plain", Uploader: testUploader, Hash: strings.Repeat("a", 64)})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("quota-only oversized upload error = %v", err)
	}
}

func TestSlowUploadDoesNotHoldQuotaLockWhileReading(t *testing.T) {
	service := testService(t)
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := service.Put(context.Background(), PutOptions{Reader: reader, Type: "text/plain", Uploader: testUploader})
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("stalled upload returned before receiving data")
	case <-time.After(25 * time.Millisecond):
	}
	second := strings.Repeat("b", 64)
	finished := make(chan error, 1)
	go func() {
		_, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("fast"), Type: "text/plain", Uploader: second})
		finished <- err
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("second upload: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second upload blocked behind stalled stream")
	}
	_, _ = writer.Write([]byte("slow"))
	_ = writer.Close()
	if err := <-done; err != nil {
		t.Fatalf("stalled upload completion: %v", err)
	}
}

func TestPrivateBlobRequiresReadAuthorization(t *testing.T) {
	service := testService(t)
	service.config.CanRead = func(context.Context, string, []string) bool { return false }
	service.config.Authorize = func(*http.Request, Action) (string, error) { return "", os.ErrPermission }
	response := record(service.Handler().ServeHTTP, httptest.NewRequest(http.MethodGet, "https://relay.test/"+strings.Repeat("a", 64), nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("private blob status = %d, want 401", response.Code)
	}
}

func TestMirrorURLRejectsPrivateAndNonHTTPS(t *testing.T) {
	for _, raw := range []string{"http://example.com/a", "https://127.0.0.1/a", "https://[::1]/a", "https://localhost/a"} {
		if err := validateMirrorURL(raw, func(context.Context, string) ([]net.IP, error) { return nil, nil }); err == nil {
			t.Errorf("validateMirrorURL(%q) succeeded", raw)
		}
	}
}

func TestMirrorBlossomURIFallsBackAndVerifiesHash(t *testing.T) {
	service := testService(t)
	body := []byte("fallback body")
	hash := sha256.Sum256(body)
	digest := hex.EncodeToString(hash[:])
	first := "https://first.example/" + digest
	author := strings.Repeat("c", 64)
	service.config.ResolveIP = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
	service.resolve = service.config.ResolveIP
	service.config.ResolveServers = func(_ context.Context, pubkey string) ([]string, error) {
		if pubkey != author {
			t.Fatalf("fallback resolved uploader %q, want URI author", pubkey)
		}
		return []string{"https://second.example"}, nil
	}
	service.config.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == first {
			return &http.Response{StatusCode: http.StatusBadGateway, Body: http.NoBody, Header: make(http.Header), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{"Content-Type": []string{"text/plain"}}, Request: req}, nil
	})}
	uri := "blossom:" + digest + "?xs=first.example&as=" + author
	request := httptest.NewRequest(http.MethodPut, "https://relay.test/mirror", strings.NewReader(`{"url":"`+uri+`"}`))
	response := record(service.Handler().ServeHTTP, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), digest) {
		t.Fatalf("fallback mirror = %d %s", response.Code, response.Body.String())
	}
}

func TestMirrorBlossomURIRejectsWrongHash(t *testing.T) {
	service := testService(t)
	wanted := strings.Repeat("a", 64)
	service.config.ResolveIP = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
	service.resolve = service.config.ResolveIP
	service.config.ResolveServers = func(context.Context, string) ([]string, error) { return []string{"https://origin.example"}, nil }
	service.config.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("wrong")), Header: make(http.Header), Request: req}, nil
	})}
	request := httptest.NewRequest(http.MethodPut, "https://relay.test/mirror", strings.NewReader(`{"url":"blossom:`+wanted+`"}`))
	response := record(service.Handler().ServeHTTP, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("wrong hash status = %d, want 409", response.Code)
	}
	if _, _, err := service.Get(context.Background(), wanted); err == nil {
		t.Fatal("wrong hash was stored")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeleteTombstonePreventsOrphanResurrection(t *testing.T) {
	service := testService(t)
	body := []byte("delete me")
	digest := sha256.Sum256(body)
	sha := hex.EncodeToString(digest[:])
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader(string(body)), Type: "text/plain", Uploader: testUploader}); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(context.Background(), sha); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(service.root, sha), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), service.config); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Get(context.Background(), sha); err == nil {
		t.Fatal("deleted orphan was resurrected")
	}
}

func TestBlobClaimsPreserveDeduplicatedOwnershipAndCursor(t *testing.T) {
	service := testService(t)
	body := "shared blob"
	first, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader(body), Type: "text/plain", Uploader: testUploader})
	if err != nil {
		t.Fatal(err)
	}
	secondUploader := strings.Repeat("b", 64)
	second, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader(body), Type: "text/plain", Uploader: secondUploader})
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatalf("deduplicated hashes differ: %q and %q", first.SHA256, second.SHA256)
	}
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader("another blob"), Type: "text/plain", Uploader: testUploader}); err != nil {
		t.Fatal(err)
	}
	page, next, err := service.ListPage(context.Background(), testUploader, 1, "")
	if err != nil || len(page) != 1 || next == "" {
		t.Fatalf("first page = %#v next=%q err=%v", page, next, err)
	}
	page, next, err = service.ListPage(context.Background(), secondUploader, 1, "")
	if err != nil || len(page) != 1 || next != "" {
		t.Fatalf("second owner page = %#v next=%q err=%v", page, next, err)
	}
	if err := service.DeleteForUploader(context.Background(), first.SHA256, testUploader); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Get(context.Background(), second.SHA256); err != nil {
		t.Fatalf("shared content removed with one claim: %v", err)
	}
	if err := service.DeleteForUploader(context.Background(), second.SHA256, secondUploader); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Get(context.Background(), second.SHA256); err == nil {
		t.Fatal("final claim deletion retained blob")
	}
}

const testUploader = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(root, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := New(context.Background(), Config{Root: root, Store: store, Authorize: func(*http.Request, Action) (string, error) { return "uploader", nil }})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func record(handler http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler(response, req)
	return response
}
