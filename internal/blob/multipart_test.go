package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestMultipartAcceptsOutOfOrderLargeChunks(t *testing.T) {
	s := testService(t)
	body := []byte(strings.Repeat("a", 70000) + strings.Repeat("b", 70000))
	digest := sha256.Sum256(body)
	sha := hex.EncodeToString(digest[:])
	patch := func(offset int64, data []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "https://relay.test/"+sha+".bin", strings.NewReader(string(data)))
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("Upload-Type", "application/octet-stream")
		r.Header.Set("Upload-Length", "140000")
		r.Header.Set("Upload-Offset", formatInt(offset))
		return record(s.Handler(), r)
	}
	if got := patch(70000, body[70000:]); got.Code != http.StatusNoContent {
		t.Fatalf("second chunk status = %d", got.Code)
	}
	if got := patch(0, body[:70000]); got.Code != http.StatusCreated {
		t.Fatalf("first chunk status = %d: %s", got.Code, got.Body.String())
	}
	entry, file, err := s.Get(context.Background(), sha)
	if err != nil || entry.Size != int64(len(body)) {
		t.Fatalf("stored multipart blob = %#v, %v", entry, err)
	}
	defer file.Close()
}

func formatInt(value int64) string { return strconv.FormatInt(value, 10) }

func TestMultipartRejectsMalformedRangesAndLength(t *testing.T) {
	s := testService(t)
	digest := sha256.Sum256([]byte("abc"))
	sha := hex.EncodeToString(digest[:])
	tests := []struct {
		name          string
		contentLength int64
		uploadLength  string
		offset        string
		want          int
	}{
		{"missing content length", -1, "3", "0", http.StatusLengthRequired},
		{"offset outside upload", 1, "3", "3", http.StatusRequestedRangeNotSatisfiable},
		{"chunk crosses upload", 2, "3", "2", http.StatusRequestedRangeNotSatisfiable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPatch, "https://relay.test/"+sha, strings.NewReader("ab"))
			r.ContentLength = tc.contentLength
			r.Header.Set("Content-Type", "application/octet-stream")
			r.Header.Set("Upload-Type", "text/plain")
			r.Header.Set("Upload-Length", tc.uploadLength)
			r.Header.Set("Upload-Offset", tc.offset)
			if got := record(s.Handler(), r).Code; got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMultipartMismatchDoesNotPublishBlob(t *testing.T) {
	s := testService(t)
	want := strings.Repeat("0", 64)
	r := httptest.NewRequest(http.MethodPatch, "https://relay.test/"+want, strings.NewReader("wrong"))
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("Upload-Type", "text/plain")
	r.Header.Set("Upload-Length", "5")
	r.Header.Set("Upload-Offset", "0")
	if got := record(s.Handler(), r).Code; got != http.StatusConflict {
		t.Fatalf("status = %d, want 409", got)
	}
	if _, _, err := s.Get(context.Background(), want); err == nil {
		t.Fatal("mismatched multipart upload was published")
	}
}

func TestMultipartOverlappingChunksReconstructExactBytes(t *testing.T) {
	s := testService(t)
	body := []byte("abcdefghij")
	digest := sha256.Sum256(body)
	sha := hex.EncodeToString(digest[:])
	request := func(offset int, chunk string) int {
		r := httptest.NewRequest(http.MethodPatch, "https://relay.test/"+sha, strings.NewReader(chunk))
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("Upload-Type", "text/plain")
		r.Header.Set("Upload-Length", "10")
		r.Header.Set("Upload-Offset", formatInt(int64(offset)))
		return record(s.Handler(), r).Code
	}
	if got := request(0, "abcdef"); got != http.StatusNoContent {
		t.Fatalf("first status = %d", got)
	}
	if got := request(4, "efghij"); got != http.StatusCreated {
		t.Fatalf("overlap status = %d", got)
	}
	_, file, err := s.Get(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil || string(got) != string(body) {
		t.Fatalf("body = %q, %v", got, err)
	}
}

func TestMultipartResumesAfterServiceRestart(t *testing.T) {
	original := testService(t)
	body := "survives restart"
	digest := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(digest[:])
	patch := func(service *Service, offset int, chunk string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPatch, "https://relay.test/"+hash, strings.NewReader(chunk))
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set("Upload-Type", "text/plain")
		request.Header.Set("Upload-Length", strconv.Itoa(len(body)))
		request.Header.Set("Upload-Offset", strconv.Itoa(offset))
		return record(service.Handler(), request)
	}
	if result := patch(original, 0, body[:5]); result.Code != 204 {
		t.Fatalf("partial: %d %s", result.Code, result.Body.String())
	}
	reopened, err := New(context.Background(), original.config)
	if err != nil {
		t.Fatal(err)
	}
	if result := patch(reopened, 5, body[5:]); result.Code != 201 {
		t.Fatalf("resumed: %d %s", result.Code, result.Body.String())
	}
}

func TestMultipartPreservesClaimMetadataAcrossChunks(t *testing.T) {
	s := testService(t)
	body := "resumable metadata"
	digest := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(digest[:])
	patch := func(offset int, chunk string, query string) int {
		r := httptest.NewRequest(http.MethodPatch, "https://relay.test/"+hash+query, strings.NewReader(chunk))
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("Upload-Type", "text/plain")
		r.Header.Set("Upload-Length", strconv.Itoa(len(body)))
		r.Header.Set("Upload-Offset", strconv.Itoa(offset))
		return record(s.Handler(), r).Code
	}
	if got := patch(0, body[:9], "?filename=message.txt&path=notes%2Fmessage.txt"); got != http.StatusNoContent {
		t.Fatalf("partial status = %d", got)
	}
	if got := patch(9, body[9:], ""); got != http.StatusCreated {
		t.Fatalf("complete status = %d", got)
	}
	entries, err := s.ListClaimMetadata(context.Background(), "uploader")
	if err != nil || len(entries) != 1 || entries[0].Name != "message.txt" || entries[0].Path != "notes/message.txt" {
		t.Fatalf("metadata = %+v, %v", entries, err)
	}
}

func TestMultipartAcceptsEmptyFile(t *testing.T) {
	service := testService(t)
	digest := sha256.Sum256(nil)
	request := httptest.NewRequest(http.MethodPatch, "https://relay.test/"+hex.EncodeToString(digest[:]), http.NoBody)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("Upload-Type", "application/octet-stream")
	request.Header.Set("Upload-Length", "0")
	request.Header.Set("Upload-Offset", "0")
	if result := record(service.Handler(), request); result.Code != 201 {
		t.Fatalf("empty: %d %s", result.Code, result.Body.String())
	}
}

func TestMultipartExistingBlobAddsUploaderClaim(t *testing.T) {
	service := testService(t)
	const content = "deduplicated"
	entry, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader(content), Uploader: "first"})
	if err != nil {
		t.Fatal(err)
	}
	request := multipartReviewRequest(entry.SHA256, int64(len(content)), 0, content[:2])
	if result := record(service.Handler(), request); result.Code != 200 {
		t.Fatalf("duplicate: %d %s", result.Code, result.Body.String())
	}
	if !service.hasClaim(context.Background(), entry.SHA256, "uploader") {
		t.Fatal("existing blob did not grant the uploader a claim")
	}
}
