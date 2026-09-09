package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/auth"
)

func TestGitTokenMintsAReusableRepositoryRootProof(t *testing.T) {
	secret := strings.Repeat("7", 64)
	t.Setenv("TINY_AGENT_KEY", secret)
	var out bytes.Buffer
	if err := gitToken([]string{"--repo", "https://relay.example/npub1owner/notes.git/info/refs?service=git-upload-pack", "--format", "value"}, &out); err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(out.String())
	validator := auth.NewValidator(time.Now)
	for _, request := range []string{"https://relay.example/npub1owner/notes.git/git-upload-pack", "https://relay.example/npub1owner/notes.git/git-receive-pack"} {
		if _, err := validator.VerifyGRASP08("Nostr "+token, request); err != nil {
			t.Fatalf("%s: %v", request, err)
		}
	}
	out.Reset()
	if err := gitToken([]string{"--repo", "https://relay.example/npub1owner/notes.git", "--format", "git"}, &out); err != nil || !strings.HasPrefix(out.String(), "http.extraHeader=Authorization: Nostr ") {
		t.Fatalf("git format: %v %q", err, out.String())
	}
	t.Setenv("TINY_AGENT_KEY", "nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5")
	out.Reset()
	if err := gitToken([]string{"--repo", "https://relay.example/npub1owner/notes.git", "--format", "value"}, &out); err != nil {
		t.Fatalf("nsec key: %v", err)
	}
	if err := gitToken([]string{"--repo", "https://relay.example/not-a-repo"}, &out); err == nil {
		t.Fatal("non-repository URL accepted")
	}
	t.Setenv("TINY_AGENT_KEY", "")
	if err := gitToken([]string{"--repo", "https://relay.example/npub1owner/notes.git"}, &out); err == nil {
		t.Fatal("missing key accepted")
	}
}
