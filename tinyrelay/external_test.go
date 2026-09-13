package tinyrelay_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExternalModuleCanUsePublicAPI compiles a consumer in a separate module.
// Keep this test independent of implementation packages so public API changes
// fail at the same boundary as a real relay integrator.
func TestExternalModuleCanUsePublicAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles an independent consumer module")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	consumer := t.TempDir()
	mod := fmt.Sprintf("module example.invalid/tinyrelay-consumer\n\ngo 1.27.1\n\nrequire github.com/FelineStateMachine/tinyrelay v0.0.0\n\nreplace github.com/FelineStateMachine/tinyrelay => %s\n", filepath.ToSlash(root))
	if err := os.WriteFile(filepath.Join(consumer, "go.mod"), []byte(mod), 0600); err != nil {
		t.Fatal(err)
	}
	source := `package consumer

import (
 "context"
 "net/http"
 "net/http/httptest"
 "path/filepath"
 "testing"
 "github.com/FelineStateMachine/tinyrelay/protocol/nostr"
 "github.com/FelineStateMachine/tinyrelay/tinyrelay"
)

type backend struct{}
func (backend) Publish(context.Context, nostr.Event, tinyrelay.Session) (string, error) { return "", nil }
func (backend) Query(context.Context, []nostr.Filter, tinyrelay.Session) ([]nostr.Event, error) { return nil, nil }
func (backend) Count(context.Context, []nostr.Filter, tinyrelay.Session) (any, error) { return 0, nil }
func (backend) CanRead(nostr.Event, tinyrelay.Session) bool { return true }
func (backend) CanReadFilter(nostr.Event, tinyrelay.Session, *nostr.Filter) bool { return true }
func (backend) QueryHints(context.Context, []nostr.Filter, tinyrelay.Session) ([]nostr.Event, []string, error) { return nil, nil, nil }
func (backend) Sync(context.Context, nostr.Filter, tinyrelay.Session) ([]tinyrelay.SyncItem, error) { return nil, nil }
func (backend) CloseSubscriptions(tinyrelay.Session) {}

func TestUse(t *testing.T) {
 ctx := context.Background()
 embedded, err := tinyrelay.New(backend{}, tinyrelay.Config{RelayURL: "ws://relay.example"})
 if err != nil { t.Fatal(err) }
 var _ http.Handler = embedded
 if err := embedded.Close(ctx); err != nil { t.Fatal(err) }

 standalone, err := tinyrelay.OpenServer(ctx, tinyrelay.ServerConfig{DataDir: filepath.Join(t.TempDir(), "data"), PublicURL: "ws://relay.example"})
 if err != nil { t.Fatal(err) }
 defer standalone.Close()
 var _ http.Handler = standalone
 response := httptest.NewRecorder()
 standalone.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://relay.example/", nil))
 if response.Code != http.StatusOK { t.Fatalf("root status = %d", response.Code) }
}
`
	if err := os.WriteFile(filepath.Join(consumer, "consumer_test.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-mod=mod", ".")
	cmd.Dir = consumer
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external consumer: %v\n%s", err, out)
	}
}

func TestPublicPackagesAvoidHostedApplicationDependencies(t *testing.T) {
	if testing.Short() {
		t.Skip("inspects production package dependencies")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-mod=mod", "-f", "{{.ImportPath}}", "-deps", "./tinyrelay", "./cmd/tinyrelay")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("list public package dependencies: %v", err)
	}
	for _, path := range strings.Fields(string(out)) {
		forbidden := []string{
			"github.com/FelineStateMachine/tinyrelay/internal/daemon",
			"github.com/FelineStateMachine/tinyrelay/internal/tinygit",
			"github.com/FelineStateMachine/tinyrelay/tinygit",
			"github.com/FelineStateMachine/tinyrelay/tinyclient",
		}
		for _, prefix := range forbidden {
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				t.Fatalf("public relay dependency imports hosted application package %q", path)
			}
		}
	}
}
