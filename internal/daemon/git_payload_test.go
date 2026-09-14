package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	gitrelay "github.com/FelineStateMachine/tinyrelay/tinygit"
)

type repeatedGitByte struct{}

func (repeatedGitByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestGitAuthorizationSpoolsPayloadAndCleansUp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	const size = 32 << 20
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(repeatedGitByte{}, size)); err != nil {
		t.Fatal(err)
	}
	proof := event.Event{Tags: [][]string{{"payload", hex.EncodeToString(hash.Sum(nil))}}}
	req := httptest.NewRequest(http.MethodPost, "http://relay.test/private.git/git-receive-pack", io.LimitReader(repeatedGitByte{}, size))
	if err := spoolGitPayload(req, proof); err != nil {
		t.Fatal(err)
	}
	body, ok := req.Body.(*gitPayloadBody)
	if !ok {
		t.Fatal("large authenticated pack was not backed by a file")
	}
	defer body.Close()
	actual := sha256.New()
	if n, err := io.Copy(actual, body); err != nil || n != size || !strings.EqualFold(hex.EncodeToString(actual.Sum(nil)), event.Tag(proof, "payload")) {
		t.Fatalf("verified payload changed: bytes=%d err=%v", n, err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(body.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authorization spool remains: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
}

func TestGitAuthorizationRejectsMismatchedPayloadWithoutLeavingSpool(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	req := httptest.NewRequest(http.MethodPost, "http://relay.test/private.git/git-receive-pack", strings.NewReader("tampered pack"))
	if err := spoolGitPayload(req, event.Event{Tags: [][]string{{"payload", strings.Repeat("0", 64)}}}); err == nil {
		t.Fatal("mismatched payload was accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed authorization left temporary files: %v %v", entries, err)
	}
}

func TestGitAuthorizationValidatesEmptyPayload(t *testing.T) {
	for _, bodyKind := range []string{"nil", "no body", "empty stream"} {
		for _, payload := range []string{"", emptyPayloadHash(), strings.Repeat("a", 64)} {
			t.Run(bodyKind+"/"+payload, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "http://relay.test/repo.git/git-receive-pack", nil)
				switch bodyKind {
				case "nil":
					req.Body = nil
				case "empty stream":
					req.Body = io.NopCloser(strings.NewReader(""))
				}
				proof := event.Event{Tags: [][]string{}}
				if payload != "" {
					proof.Tags = append(proof.Tags, []string{"payload", payload})
				}
				err := spoolGitPayload(req, proof)
				wantErr := payload != "" && payload != emptyPayloadHash()
				if (err != nil) != wantErr {
					t.Fatalf("validate empty payload: %v, want error=%v", err, wantErr)
				}
			})
		}
	}
}

type unreadGitBody struct{ read bool }

func (b *unreadGitBody) Read([]byte) (int, error) {
	b.read = true
	return 0, errors.New("unauthenticated body should not be read")
}
func (*unreadGitBody) Close() error { return nil }

func TestGitAuthorizationRejectsUnsignedUploadBeforeReadingBody(t *testing.T) {
	_, tenant := testTenant(t)
	p := tenant.Policy()
	p.Features.Grasp = true
	if err := tenant.applyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	body := &unreadGitBody{}
	req := httptest.NewRequest(http.MethodPost, "http://relay.test/private.git/git-receive-pack", nil)
	req.Body = body
	if err := tenant.authorizeGit(context.Background(), req, gitrelay.Repository{Owner: p.Owner, Private: true}); err == nil {
		t.Fatal("unsigned private upload accepted")
	}
	if body.read {
		t.Fatal("unsigned upload body was consumed")
	}
}
