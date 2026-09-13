package protocol_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/FelineStateMachine/tinyrelay"

// Run a real consumer outside tinyrelay's module so Go's internal import
// restriction, public type identity and standalone dependencies are checked.
func TestExternalNostrConsumer(t *testing.T) {
	runConsumer(t, "nostr")
}

func TestExternalAuthConsumer(t *testing.T) {
	runConsumer(t, "auth")
}

func runConsumer(t *testing.T, fixture string) {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var goVersion string
	for _, line := range strings.Split(string(mod), "\n") {
		if strings.HasPrefix(line, "go ") {
			goVersion = strings.TrimPrefix(line, "go ")
		}
	}
	if goVersion == "" {
		t.Fatal("module has no Go version")
	}
	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", []byte("module example.com/protocol-consumer\n\ngo "+goVersion+"\n\nrequire "+modulePath+" v0.0.0\nreplace "+modulePath+" => "+strconv.Quote(filepath.ToSlash(root))+"\n"))
	for _, file := range []struct{ src, dst string }{
		{filepath.Join(root, "go.sum"), "go.sum"},
		{filepath.Join("testdata", fixture, "consumer_test.go"), "consumer_test.go"},
	} {
		data, err := os.ReadFile(file.src)
		if err != nil {
			t.Fatal(err)
		}
		write(file.dst, data)
	}
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "test", "-mod=mod", "-count=1", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external %s consumer: %v\n%s", fixture, err, out)
	}
}
