package blob

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func multipartReviewRequest(sha string, length, offset int64, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPatch, "https://relay.test/"+sha, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("Upload-Type", "application/octet-stream")
	r.Header.Set("Upload-Length", formatInt(length))
	r.Header.Set("Upload-Offset", formatInt(offset))
	return r
}

func TestMultipartReviewConcurrentChunksProduceOneBlob(t *testing.T) {
	s := testService(t)
	body := []byte(strings.Repeat("a", 70000) + strings.Repeat("b", 70000))
	digest := sha256.Sum256(body)
	sha := hex.EncodeToString(digest[:])
	parts := []struct {
		offset int64
		body   string
	}{
		{70000, string(body[70000:])},
		{0, string(body[:70000])},
	}
	responses := make([]*httptest.ResponseRecorder, len(parts))
	var wg sync.WaitGroup
	for i, part := range parts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses[i] = record(s.Handler(), multipartReviewRequest(sha, int64(len(body)), part.offset, part.body))
		}()
	}
	wg.Wait()
	var created, partial int
	for _, response := range responses {
		switch response.Code {
		case http.StatusCreated:
			created++
		case http.StatusNoContent:
			partial++
		default:
			t.Fatalf("concurrent chunk status = %d: %s", response.Code, response.Body.String())
		}
	}
	if created != 1 || partial != 1 {
		t.Fatalf("concurrent chunk results created=%d partial=%d", created, partial)
	}
	entry, file, err := s.Get(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if entry.Size != int64(len(body)) {
		t.Fatalf("stored size = %d, want %d", entry.Size, len(body))
	}
	got, err := io.ReadAll(file)
	if err != nil || string(got) != string(body) {
		t.Fatalf("stored body mismatch: %v", err)
	}
}

func TestMultipartReviewStalledBodyDoesNotBlockOrdinaryUpload(t *testing.T) {
	s := testService(t)
	body := "stalled"
	digest := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(digest[:])
	started := make(chan struct{})
	release := make(chan struct{})
	r := multipartReviewRequest(sha, int64(len(body)), 0, body)
	r.Body = &multipartBlockingBody{Reader: strings.NewReader(body), started: started, release: release}
	r.ContentLength = int64(len(body))
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- record(s.Handler(), r) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("multipart body was not read")
	}
	if _, err := s.Put(context.Background(), PutOptions{Reader: strings.NewReader("ordinary"), Type: "text/plain", Uploader: "uploader"}); err != nil {
		t.Fatalf("ordinary upload blocked by stalled multipart body: %v", err)
	}
	close(release)
	select {
	case response := <-result:
		if response.Code != http.StatusCreated {
			t.Fatalf("multipart completion status = %d", response.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled multipart request did not finish")
	}
}

func TestMultipartReviewStalledHashDoesNotBlockOrdinaryUpload(t *testing.T) {
	s := testService(t)
	body := "hash-paused"
	digest := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(digest[:])
	started := make(chan struct{})
	release := make(chan struct{})
	s.verifyMultipartHook = func(path, hash string, size int64) error {
		close(started)
		<-release
		return verifyMultipart(path, hash, size)
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	r := multipartReviewRequest(sha, int64(len(body)), 0, body)
	go func() { result <- record(s.Handler(), r) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("multipart hash was not started")
	}
	ordinary := make(chan error, 1)
	go func() {
		_, err := s.Put(context.Background(), PutOptions{Reader: strings.NewReader("ordinary"), Type: "text/plain", Uploader: "other"})
		ordinary <- err
	}()
	select {
	case err := <-ordinary:
		if err != nil {
			t.Fatalf("ordinary upload while multipart hash paused: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary upload blocked by multipart hashing")
	}
	close(release)
	select {
	case response := <-result:
		if response.Code != http.StatusCreated {
			t.Fatalf("multipart completion status = %d: %s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("multipart hash did not finish")
	}
}

func TestMultipartReviewStalledHashDoesNotBlockAnotherSession(t *testing.T) {
	s := testService(t)
	body := "hash-paused"
	digest := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(digest[:])
	started := make(chan struct{})
	release := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	s.verifyMultipartHook = func(path, hash string, size int64) error {
		close(started)
		<-release
		return verifyMultipart(path, hash, size)
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- record(s.Handler(), multipartReviewRequest(sha, int64(len(body)), 0, body)) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("multipart hash did not start")
	}
	// A different session must reserve space and commit a partial chunk while
	// the first session hashes. Ordinary PUT does not take multipartMu and
	// would not detect the tenant-wide finalization lock regression.
	secondBody := strings.Repeat("b", 70000)
	secondDigest := sha256.Sum256([]byte(secondBody + "remaining"))
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		second <- record(s.Handler(), multipartReviewRequest(hex.EncodeToString(secondDigest[:]), int64(len(secondBody)+len("remaining")), 0, secondBody))
	}()
	select {
	case response := <-second:
		if response.Code != http.StatusNoContent {
			t.Fatalf("second session = %d: %s", response.Code, response.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("another multipart session blocked by hashing")
	}
	unblock.Do(func() { close(release) })
	select {
	case response := <-first:
		if response.Code != http.StatusCreated {
			t.Fatalf("finalizing session = %d: %s", response.Code, response.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("finalizing session did not finish")
	}
}

func TestMultipartReviewFinalizingSessionRejectsConcurrentChunk(t *testing.T) {
	s := testService(t)
	body := "same-session"
	digest := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(digest[:])
	started := make(chan struct{})
	release := make(chan struct{})
	s.verifyMultipartHook = func(path, hash string, size int64) error {
		close(started)
		<-release
		return verifyMultipart(path, hash, size)
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- record(s.Handler(), multipartReviewRequest(sha, int64(len(body)), 0, body)) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("multipart hash was not started")
	}
	second := record(s.Handler(), multipartReviewRequest(sha, int64(len(body)), 0, body))
	if second.Code != http.StatusConflict {
		t.Fatalf("concurrent same-session chunk status = %d: %s", second.Code, second.Body.String())
	}
	close(release)
	if response := <-first; response.Code != http.StatusCreated {
		t.Fatalf("finalizing upload status = %d: %s", response.Code, response.Body.String())
	}
}

func TestMultipartReviewCleanupDoesNotRemoveActiveFinalization(t *testing.T) {
	s := testService(t)
	body := "cleanup-paused"
	digest := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(digest[:])
	started := make(chan struct{})
	release := make(chan struct{})
	s.verifyMultipartHook = func(path, hash string, size int64) error {
		close(started)
		<-release
		return verifyMultipart(path, hash, size)
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- record(s.Handler(), multipartReviewRequest(sha, int64(len(body)), 0, body)) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("multipart hash was not started")
	}
	if _, err := s.store.DB().Exec("UPDATE multipart_uploads SET last_seen=0"); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupMultipart(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := multipartID(sha, "uploader")
	if _, err := os.Stat(s.multipartPath(id)); err != nil {
		t.Fatalf("active multipart file removed during cleanup: %v", err)
	}
	var found int
	if err := s.store.DB().QueryRow("SELECT COUNT(*) FROM multipart_uploads WHERE id=?", id).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatal("active multipart reservation removed during cleanup")
	}
	close(release)
	if response := <-result; response.Code != http.StatusCreated {
		t.Fatalf("finalizing upload status = %d: %s", response.Code, response.Body.String())
	}
}

func TestMultipartReviewPolicyRecheckedAfterHash(t *testing.T) {
	s := testService(t)
	body := "policy-after-hash"
	digest := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(digest[:])
	started := make(chan struct{})
	release := make(chan struct{})
	s.verifyMultipartHook = func(path, hash string, size int64) error {
		close(started)
		<-release
		return verifyMultipart(path, hash, size)
	}
	s.config.Limits = func() Limits { return Limits{MaxFileBytes: int64(len(body))} }
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- record(s.Handler(), multipartReviewRequest(sha, int64(len(body)), 0, body)) }()
	<-started
	s.config.Limits = func() Limits { return Limits{MaxFileBytes: int64(len(body) - 1)} }
	close(release)
	if response := <-result; response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("policy-changed upload status = %d: %s", response.Code, response.Body.String())
	}
	if _, _, err := s.Get(context.Background(), sha); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("policy-rejected upload lookup = %v", err)
	}
}

func TestMultipartReviewCanceledHashCanBeRetried(t *testing.T) {
	s := testService(t)
	body := "cancel-and-retry"
	digest := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(digest[:])
	started := make(chan struct{})
	release := make(chan struct{})
	s.verifyMultipartHook = func(path, hash string, size int64) error {
		close(started)
		<-release
		return verifyMultipart(path, hash, size)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := multipartReviewRequest(sha, int64(len(body)), 0, body).WithContext(ctx)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- record(s.Handler(), r) }()
	<-started
	cancel()
	close(release)
	<-result
	s.verifyMultipartHook = nil
	if response := record(s.Handler(), multipartReviewRequest(sha, int64(len(body)), 0, body)); response.Code != http.StatusCreated {
		t.Fatalf("retry after canceled hash status = %d: %s", response.Code, response.Body.String())
	}
}

type multipartBlockingBody struct {
	io.Reader
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *multipartBlockingBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.Reader.Read(p)
}

func (b *multipartBlockingBody) Close() error { return nil }

func TestMultipartReviewReservationsBlockUploadsAndReleaseOnExpiry(t *testing.T) {
	s := testService(t)
	s.config.Limits = func() Limits { return Limits{UserStorageBytes: 8} }
	full := "12345678"
	digest := sha256.Sum256([]byte(full))
	sha := hex.EncodeToString(digest[:])
	partial := multipartReviewRequest(sha, int64(len(full)), 0, full[:4])
	partial.ContentLength = 4
	if response := record(s.Handler(), partial); response.Code != http.StatusNoContent {
		t.Fatalf("partial reservation status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := s.Put(context.Background(), PutOptions{Reader: strings.NewReader("1234"), Type: "text/plain", Uploader: "uploader"}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("ordinary upload after reservation = %v", err)
	}
	other := "abcdefgh"
	otherDigest := sha256.Sum256([]byte(other))
	second := multipartReviewRequest(hex.EncodeToString(otherDigest[:]), int64(len(other)), 0, other[:4])
	second.ContentLength = 4
	if response := record(s.Handler(), second); response.Code != http.StatusForbidden {
		t.Fatalf("second reserved upload status = %d", response.Code)
	}
	if _, err := s.store.DB().Exec("UPDATE multipart_uploads SET last_seen=0"); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupMultipart(context.Background()); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(s.root, ".multipart-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("expired multipart files remain: %v", matches)
	}
	if _, err := s.Put(context.Background(), PutOptions{Reader: strings.NewReader("1234"), Type: "text/plain", Uploader: "uploader"}); err != nil {
		t.Fatalf("ordinary upload after expiry: %v", err)
	}
}

func TestMultipartReviewHashMismatchReleasesReservation(t *testing.T) {
	s := testService(t)
	s.config.Limits = func() Limits { return Limits{UserStorageBytes: 5} }
	wrong := strings.Repeat("a", 64)
	r := multipartReviewRequest(wrong, 5, 0, "wrong")
	r.ContentLength = 5
	if response := record(s.Handler(), r); response.Code != http.StatusConflict {
		t.Fatalf("hash mismatch status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := s.Put(context.Background(), PutOptions{Reader: strings.NewReader("right"), Type: "text/plain", Uploader: "uploader"}); err != nil {
		t.Fatalf("upload after mismatch reservation: %v", err)
	}
}

func TestMultipartReviewExistingBlobCannotBypassModerationBlock(t *testing.T) {
	s := testService(t)
	body := "blocked"
	digest := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(digest[:])
	if _, err := s.Put(context.Background(), PutOptions{Reader: strings.NewReader(body), Type: "text/plain"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.DB().Exec("INSERT INTO blob_blocks(sha256,reason,blocked_at) VALUES(?,?,?)", sha, "review", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	r := multipartReviewRequest(sha, int64(len(body)), 0, body)
	r.ContentLength = int64(len(body))
	if response := record(s.Handler(), r); response.Code != http.StatusForbidden {
		t.Fatalf("blocked existing blob status = %d: %s", response.Code, response.Body.String())
	}
}
