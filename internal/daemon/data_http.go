package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
)

// tryDataHTTP handles owner-only import and portable data operations. The
// caller should invoke it after tenant/session setup and before the public UI.
func (t *Tenant) tryDataHTTP(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/import":
		t.importHTTP(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/backups/preview":
		t.backupPreviewHTTP(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/backups/restore":
		t.backupRestoreHTTP(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/dumps/"):
		t.downloadArtifactHTTP(w, r, ".jsonl")
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/backups/"):
		t.downloadArtifactHTTP(w, r, ".backup.json")
	default:
		return false
	}
	return true
}

func (t *Tenant) ownerDataAuth(r *http.Request, body []byte, cookieAllowed bool) error {
	actor := ""
	if r.Header.Get("Authorization") != "" {
		s, err := t.session(r, body)
		if err != nil {
			return err
		}
		if len(s.PubKeys) == 1 {
			actor = s.PubKeys[0]
		}
	} else if cookieAllowed {
		var err error
		actor, err = t.cookieActor(r)
		if err != nil {
			return err
		}
	}
	if actor == "" || actor != t.Policy().Owner {
		return errors.New("restricted: owner required")
	}
	banned, err := t.community.IsBanned(r.Context(), actor)
	if err != nil {
		return err
	}
	if banned {
		return errors.New("blocked: this pubkey is banned")
	}
	return nil
}

func (t *Tenant) importHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := t.requestBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := t.ownerDataAuth(r, body, false); err != nil {
		writeJSON(w, statusFor(err), map[string]string{"error": err.Error()})
		return
	}
	events, err := parseJSONLEvents(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	for i, item := range events {
		if err := t.ingest(r.Context(), item, replication.OriginImport); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "stored": i})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"imported": len(events)}})
}

func readUnbounded(reader io.Reader) ([]byte, error) {
	return io.ReadAll(reader)
}

func parseJSONLEvents(raw []byte) ([]event.Event, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), len(raw)+1)
	var out []event.Event
	for line := 1; scanner.Scan(); line++ {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var item event.Event
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			return nil, fmt.Errorf("invalid import line %d: %w", line, err)
		}
		if err := event.Validate(item); err != nil {
			return nil, fmt.Errorf("invalid import line %d: %w", line, err)
		}
		out = append(out, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tenant) readBackup(body []byte) (replication.BackupArchive, error) {
	return replication.ReadCompatibleBackup(bytes.NewReader(body))
}

func (t *Tenant) backupPreviewHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := t.requestBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := t.ownerDataAuth(r, body, false); err != nil {
		writeJSON(w, statusFor(err), map[string]string{"error": err.Error()})
		return
	}
	archive, err := t.readBackup(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	owner, err := backupOwner(archive)
	if err != nil || owner != t.Policy().Owner {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "backup owner does not match this relay"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"format": archive.Format, "createdAt": archive.Created, "events": len(archive.Events), "hasState": len(archive.State.Database) > 0 || len(archive.State.Config) > 0}})
}

func (t *Tenant) backupRestoreHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := t.requestBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := t.ownerDataAuth(r, body, false); err != nil {
		writeJSON(w, statusFor(err), map[string]string{"error": err.Error()})
		return
	}
	archive, err := t.readBackup(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, done, admissionErr := t.beginMaintenance(r.Context())
	if admissionErr != nil {
		http.Error(w, admissionErr.Error(), http.StatusServiceUnavailable)
		return
	}
	defer done()
	r = r.WithContext(ctx)
	owner, err := backupOwner(archive)
	if err != nil || owner != t.Policy().Owner {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "backup owner does not match this relay"})
		return
	}
	fresh, err := t.restoreTargetFresh(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !fresh {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "restore target is not fresh"})
		return
	}
	count, err := replication.RestoreBackupWithProvider(r.Context(), t.store, t.ReplicationBackupProvider(), archive, time.Now().Unix())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "restored": count})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"restored": count}})
}

func (t *Tenant) downloadArtifactHTTP(w http.ResponseWriter, r *http.Request, suffix string) {
	if err := t.ownerDataAuth(r, nil, true); err != nil {
		writeJSON(w, statusFor(err), map[string]string{"error": err.Error()})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/dumps/")
	if suffix == ".backup.json" {
		name = strings.TrimPrefix(r.URL.Path, "/backups/")
	}
	if name != filepath.Base(name) || name == "." || name == "" || !strings.HasSuffix(name, suffix) || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	root, err := filepath.EvalSymlinks(t.meta.Paths.Root)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file := filepath.Join(root, name)
	resolved, err := filepath.EvalSymlinks(file)
	if err != nil || !withinPath(root, resolved) {
		http.NotFound(w, r)
		return
	}
	handle, err := os.Open(resolved)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, info.ModTime(), handle)
}

func backupOwner(archive replication.BackupArchive) (string, error) {
	var config struct {
		Owner  string `json:"owner"`
		Format string `json:"format"`
	}
	if err := json.Unmarshal(archive.State.Config, &config); err != nil {
		return "", errors.New("backup owner is missing")
	}
	owner := config.Owner
	if strings.HasPrefix(config.Format, "bind.ws/relay-config/") {
		var identity struct {
			Owner string `json:"owner"`
		}
		if err := json.Unmarshal(archive.State.Identity, &identity); err != nil {
			return "", errors.New("backup owner is missing")
		}
		owner = identity.Owner
	}
	if len(owner) != 64 || strings.Trim(owner, "0123456789abcdef") != "" {
		return "", errors.New("backup owner is invalid")
	}
	return owner, nil
}

// restoreTargetFresh deliberately counts stored rows, not visible query results:
// hidden, pending, private, or expired user events still make a target nonempty.
// Only the new relay's automatically signed discovery/membership records are
// exempt: permanent ownership creates those before the restore UI can open.
// The caller holds exclusive maintenance admission through the restore.
func (t *Tenant) restoreTargetFresh(ctx context.Context) (bool, error) {
	var occupied bool
	if err := t.store.DB().QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM events WHERE pubkey<>? OR kind NOT IN (0,30166,13534,30078,39000,39001,39002,39003,33534,8000,9000)
 UNION ALL SELECT 1 FROM blobs
 UNION ALL SELECT 1 FROM community_members WHERE pubkey<>?
 )`, t.records.PublicKey(), t.Policy().Owner).Scan(&occupied); err != nil {
		return false, err
	}
	if occupied {
		return false, nil
	}
	for _, root := range []string{filepath.Join(t.meta.Paths.Root, "blobs"), t.meta.Paths.Git} {
		found := false
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if path == filepath.Join(t.meta.Paths.Git, ".tinyrelay", "grasp-progress.json") {
				return nil
			}
			if !entry.IsDir() {
				found = true
				return filepath.SkipAll
			}
			return nil
		})
		if err != nil {
			return false, err
		}
		if found {
			return false, nil
		}
	}
	return true, nil
}

func withinPath(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
