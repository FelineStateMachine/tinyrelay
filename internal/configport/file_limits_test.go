package configport

import (
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

func TestImportFileLimits(t *testing.T) {
	doc, err := Parse([]byte(`{"format":"bind.ws/relay-config/2","policy":{"fileLimits":{"maxFileBytes":1024,"userStorageBytes":4096}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Warnings) != 0 {
		t.Fatalf("file limits should be portable: %v", doc.Warnings)
	}
	got, err := finalPolicy(policy.Defaults("owner"), doc, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.FileLimits.MaxFileBytes != 1024 || got.FileLimits.UserStorageBytes != 4096 {
		t.Fatalf("file limits lost during import: %+v", got.FileLimits)
	}
}
