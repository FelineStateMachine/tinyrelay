package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadHeadAcceptsEmptyFileAtFullQuota(t *testing.T) {
	s := testService(t)
	s.config.Limits = func() Limits { return Limits{MaxFileBytes: 2, UserStorageBytes: 2} }
	if _, err := s.Put(context.Background(), PutOptions{Reader: strings.NewReader("ab"), Type: "text/plain", Uploader: testUploader}); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(nil)
	hash := hex.EncodeToString(digest[:])
	for _, phase := range []string{"before upload", "after upload"} {
		req := httptest.NewRequest(http.MethodHead, "https://relay.test/upload", nil)
		req.Header.Set("X-SHA-256", hash)
		req.Header.Set("X-Content-Length", "0")
		req.Header.Set("X-Content-Type", "application/octet-stream")
		response := record(s.Handler(), req)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: empty preflight = %d %s", phase, response.Code, response.Body.String())
		}
		if phase == "before upload" {
			upload := record(s.Handler(), httptest.NewRequest(http.MethodPut, "https://relay.test/"+hash, http.NoBody))
			if upload.Code != http.StatusCreated {
				t.Fatalf("empty PUT = %d %s", upload.Code, upload.Body.String())
			}
		}
	}
	stored, file, err := s.Get(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if stored.Size != 0 {
		t.Fatalf("empty stored blob = %+v, %v", stored, err)
	}
	usage, err := s.quotaUsage(context.Background(), testUploader)
	if err != nil || usage != 2 {
		t.Fatalf("empty file changed usage: %d, %v", usage, err)
	}
}

func TestUploadHeadRequiresNonnegativeLength(t *testing.T) {
	for _, tc := range []struct {
		value string
		code  int
	}{
		{"", http.StatusLengthRequired}, {"-1", http.StatusBadRequest},
		{"no", http.StatusBadRequest}, {"9223372036854775808", http.StatusBadRequest},
		{"0", http.StatusOK}, {"1", http.StatusOK},
	} {
		t.Run(tc.value, func(t *testing.T) {
			s := testService(t)
			req := httptest.NewRequest(http.MethodHead, "https://relay.test/upload", nil)
			req.Header.Set("X-SHA-256", strings.Repeat("a", 64))
			req.Header.Set("X-Content-Type", "text/plain")
			req.Header.Set("X-Content-Length", tc.value)
			if response := record(s.Handler(), req); response.Code != tc.code {
				t.Fatalf("length %q = %d, want %d", tc.value, response.Code, tc.code)
			}
		})
	}
}
