package policy

import (
	"encoding/json"
	"testing"
)

func TestFileLimitsPatchAndValidation(t *testing.T) {
	p := Defaults("owner")
	p.FileLimits = FileLimits{MaxFileBytes: 1024, UserStorageBytes: 4096}
	updated, err := Patch(p, map[string]json.RawMessage{"fileLimits": json.RawMessage(`{"maxFileBytes":2048}`)})
	if err != nil {
		t.Fatal(err)
	}
	if updated.FileLimits.MaxFileBytes != 2048 || updated.FileLimits.UserStorageBytes != 4096 {
		t.Fatalf("partial limit patch lost settings: %+v", updated.FileLimits)
	}
	for _, patch := range []string{`{"maxFileBytes":-1}`, `{"userStorageBytes":-1}`} {
		if _, err := Patch(p, map[string]json.RawMessage{"fileLimits": json.RawMessage(patch)}); err == nil {
			t.Fatalf("accepted negative limit: %s", patch)
		}
	}
	updated, err = Patch(p, map[string]json.RawMessage{"fileLimits": json.RawMessage(`{"maxFileBytes":0,"userStorageBytes":0}`)})
	if err != nil || updated.FileLimits != (FileLimits{}) {
		t.Fatalf("zero should disable limits: %+v, %v", updated.FileLimits, err)
	}
}
