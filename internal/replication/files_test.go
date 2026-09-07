package replication

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteArtifactFailurePreservesPreviousFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.json")
	const previous = "previous artifact\n"
	if err := os.WriteFile(path, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("generation failed")
	if err := writeArtifact(path, func(w io.Writer) error {
		if _, err := w.Write([]byte("partial replacement")); err != nil {
			return err
		}
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("writeArtifact error = %v, want %v", err, wantErr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != previous {
		t.Fatalf("previous artifact changed: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "relay.json" {
		t.Fatalf("staging file left behind: %#v", entries)
	}
}
