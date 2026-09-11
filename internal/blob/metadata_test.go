package blob

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadStoresClaimScopedNameAndPath(t *testing.T) {
	s := testService(t)
	body := "same bytes"
	request := httptest.NewRequest(http.MethodPut, "https://relay.test/upload?filename=note.txt&path=docs%2Fnote.txt", strings.NewReader(body))
	request.Header.Set("Content-Type", "text/plain")
	if response := record(s.Handler(), request); response.Code != http.StatusOK {
		t.Fatalf("upload status = %d: %s", response.Code, response.Body.String())
	}
	entries, err := s.ListClaimMetadata(context.Background(), "uploader")
	if err != nil || len(entries) != 1 || entries[0].Name != "note.txt" || entries[0].Path != "docs/note.txt" {
		t.Fatalf("metadata = %+v, %v", entries, err)
	}
	other := strings.Repeat("b", 64)
	if _, err := s.Put(context.Background(), PutOptions{Reader: strings.NewReader(body), Type: "text/plain", Uploader: other, Name: "other.txt", Path: "other.txt"}); err != nil {
		t.Fatal(err)
	}
	entries, err = s.ListClaimMetadata(context.Background(), other)
	if err != nil || len(entries) != 1 || entries[0].Name != "other.txt" {
		t.Fatalf("deduplicated metadata = %+v, %v", entries, err)
	}
}

func TestBlobAccessIsDurableAndCannotChangeOnDeduplication(t *testing.T) {
	s := testService(t)
	ctx := context.Background()
	entry, err := s.Put(ctx, PutOptions{Reader: strings.NewReader("member bytes"), Type: "text/plain", Uploader: "uploader", Access: AccessMembers})
	if err != nil {
		t.Fatal(err)
	}
	if access, err := s.BlobAccess(ctx, entry.SHA256); err != nil || access != AccessMembers {
		t.Fatalf("access = %q, %v", access, err)
	}
	if _, err := s.Put(ctx, PutOptions{Reader: strings.NewReader("member bytes"), Type: "text/plain", Uploader: "other", Access: AccessPublic}); !errors.Is(err, ErrAccessConflict) {
		t.Fatalf("public deduplication error = %v", err)
	}
	if access, err := s.BlobAccess(ctx, entry.SHA256); err != nil || access != AccessMembers {
		t.Fatalf("access changed after rejected deduplication = %q, %v", access, err)
	}
}

func TestValidateClaimPath(t *testing.T) {
	for _, path := range []string{"", "file.txt", "dir/file.txt"} {
		if _, err := validateClaimPath(path); err != nil {
			t.Errorf("validateClaimPath(%q): %v", path, err)
		}
	}
	for _, path := range []string{"/file.txt", "../file.txt", "dir/../../file.txt", "dir/../secret.txt", "dir/./file.txt", "dir//file.txt", "dir\\file.txt", "dir/", "a\x00b"} {
		if _, err := validateClaimPath(path); err == nil {
			t.Errorf("validateClaimPath(%q) succeeded", path)
		}
	}
	if !errors.Is(validateClaimName("dir/file.txt"), errInvalidClaimName) {
		t.Fatal("path was accepted as a filename")
	}
	if _, _, err := claimMetadata("other.txt", "dir/file.txt"); !errors.Is(err, errMismatchedClaimName) {
		t.Fatalf("mismatched filename accepted: %v", err)
	}
	if name, cleanPath, err := claimMetadata("", "dir/file.txt"); err != nil || name != "file.txt" || cleanPath != "dir/file.txt" {
		t.Fatalf("path basename metadata = %q, %q, %v", name, cleanPath, err)
	}
	if err := validateClaimName(string([]byte{0xff})); !errors.Is(err, errInvalidClaimName) {
		t.Fatalf("invalid UTF-8 filename accepted: %v", err)
	}
}

func TestIdenticalFilesPreserveDistinctFolderPaths(t *testing.T) {
	s := testService(t)
	ctx := context.Background()
	for _, filePath := range []string{"one/note.txt", "two/note.txt", "one/note.txt"} {
		if _, err := s.Put(ctx, PutOptions{Reader: strings.NewReader("same bytes"), Type: "text/plain", Uploader: "uploader", Name: "note.txt", Path: filePath}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.ListClaimMetadata(ctx, "uploader")
	if err != nil || len(entries) != 2 {
		t.Fatalf("distinct paths lost or repeated: %#v, %v", entries, err)
	}
	if entries[0].SHA256 != entries[1].SHA256 || entries[0].Path == entries[1].Path {
		t.Fatalf("same content should retain both paths: %#v", entries)
	}
}
