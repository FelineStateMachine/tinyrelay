package tinygit_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestExternalModuleCanUsePublicAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles an independent consumer module")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	consumer := t.TempDir()
	mod := fmt.Sprintf("module example.invalid/tinygit-consumer\n\ngo 1.27.1\n\nrequire github.com/FelineStateMachine/tinyrelay v0.0.0\nreplace github.com/FelineStateMachine/tinyrelay => %s\n", filepath.ToSlash(root))
	if err := os.WriteFile(filepath.Join(consumer, "go.mod"), []byte(mod), 0600); err != nil {
		t.Fatal(err)
	}
	source := `package consumer
import (
 "context"
 "net/http"
 "path/filepath"
 "strings"
 "testing"
 "github.com/FelineStateMachine/tinyrelay/tinygit"
)
func TestUse(t *testing.T) {
 ctx := context.Background()
 dir := t.TempDir()
 var store *tinygit.Store
 var err error
 store, err = tinygit.OpenStore(ctx, filepath.Join(dir, "events.db"))
 if err != nil { t.Fatal(err) }
 defer store.Close()
 p := tinygit.DefaultPolicy("")
 g, err := tinygit.New(tinygit.Config{Store: store, Root: filepath.Join(dir, "git"), Policy: func() tinygit.Policy { return p }, Authorize: func(context.Context, tinygit.Event, tinygit.Repository) error { return nil }})
 if err != nil { t.Fatal(err) }
 var _ http.Handler = g
 if err := g.Publish(ctx, tinygit.Event{}); err == nil { t.Fatal("invalid event accepted") }
 s, err := tinygit.OpenServer(ctx, tinygit.ServerConfig{DataDir: t.TempDir(), Owner: strings.Repeat("a", 64)})
 if err != nil { t.Fatal(err) }
 defer s.Close()
 var _ http.Handler = s
}
`
	if err := os.WriteFile(filepath.Join(consumer, "consumer_test.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-mod=mod", ".")
	cmd.Dir = consumer
	// Dependencies are already in the build cache. Never use this boundary test
	// as a reason to make a network request or change the root module files.
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external consumer: %v\n%s", err, out)
	}
}
