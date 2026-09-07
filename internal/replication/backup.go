package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const BackupFormat = "tinyrelay/backup/1"

type BackupArchive struct {
	Format  string        `json:"format"`
	Events  []event.Event `json:"events"`
	State   BackupState   `json:"state"`
	SHA256  string        `json:"sha256"`
	Created int64         `json:"created_at"`
}

type BackupState struct {
	Config    json.RawMessage `json:"config,omitempty"`
	Community json.RawMessage `json:"community,omitempty"`
	Identity  json.RawMessage `json:"identity,omitempty"`
	Blobs     []BackupObject  `json:"blobs,omitempty"`
	Sites     []BackupObject  `json:"sites,omitempty"`
	Git       []BackupObject  `json:"git,omitempty"`
	GitSQL    json.RawMessage `json:"sqlGit,omitempty"`
	Database  []BackupTable   `json:"database,omitempty"`
}

type BackupTable struct {
	Name    string              `json:"name"`
	Columns []string            `json:"columns"`
	Rows    [][]json.RawMessage `json:"rows"`
}

type BackupObject struct {
	Name     string `json:"name"`
	SHA256   string `json:"sha256"`
	Content  []byte `json:"content"`
	Type     string `json:"type,omitempty"`
	Uploader string `json:"uploader,omitempty"`
	Uploaded int64  `json:"uploaded,omitempty"`
}

type BackupProvider interface {
	Snapshot(context.Context) (BackupState, error)
	Restore(context.Context, BackupState) error
}

type FullBackupProvider interface {
	BackupProvider
	SnapshotArchive(context.Context, int64) (BackupArchive, error)
}

type RestoreFinalizer interface {
	FinalizeRestore(context.Context) error
}

func CreateBackup(ctx context.Context, store *storage.Store, now int64) (BackupArchive, error) {
	rows, err := store.After(ctx, 0, event.Filter{Tags: map[string][]string{}}, storage.QueryOptions{Now: now, Access: storage.Access{All: true}, Limit: 0})
	if err != nil {
		return BackupArchive{}, err
	}
	archive := BackupArchive{Format: BackupFormat, Created: now, Events: make([]event.Event, 0, len(rows))}
	for _, row := range rows {
		archive.Events = append(archive.Events, row.Event)
	}
	return sealBackup(archive)
}

func CreateBackupWithProvider(ctx context.Context, store *storage.Store, provider BackupProvider, now int64) (BackupArchive, error) {
	if full, ok := provider.(FullBackupProvider); ok {
		return full.SnapshotArchive(ctx, now)
	}
	archive, err := CreateBackup(ctx, store, now)
	if err != nil || provider == nil {
		return archive, err
	}
	state, err := provider.Snapshot(ctx)
	if err != nil {
		return BackupArchive{}, err
	}
	archive.State = state
	return sealBackup(archive)
}

func SealBackup(archive BackupArchive) (BackupArchive, error) { return sealBackup(archive) }

func WriteBackup(ctx context.Context, store *storage.Store, output io.Writer, now int64) error {
	archive, err := CreateBackup(ctx, store, now)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(archive)
}

func ReadBackup(input io.Reader) (BackupArchive, error) {
	var archive BackupArchive
	decoder := json.NewDecoder(input)
	if err := decoder.Decode(&archive); err != nil {
		return archive, fmt.Errorf("replication: decode backup: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return archive, errors.New("replication: trailing JSON after backup")
		}
		return archive, fmt.Errorf("replication: trailing backup data: %w", err)
	}
	if archive.Format != BackupFormat {
		return archive, errors.New("replication: unsupported backup format")
	}
	sealed, err := sealBackup(BackupArchive{Format: archive.Format, Events: archive.Events, State: archive.State, Created: archive.Created})
	if err != nil || sealed.SHA256 != archive.SHA256 {
		return archive, errors.New("replication: backup integrity check failed")
	}
	return archive, nil
}

// ReadCompatibleBackup accepts both the native archive and bind.ws/relay-backup/1.
// The legacy shape is normalized before restore so callers have one validation path.
func ReadCompatibleBackup(input io.Reader) (BackupArchive, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return BackupArchive{}, err
	}
	var envelope struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return BackupArchive{}, fmt.Errorf("replication: decode backup envelope: %w", err)
	}
	if envelope.Format == BackupFormat {
		return ReadBackup(bytes.NewReader(data))
	}
	if envelope.Format != "bind.ws/relay-backup/1" {
		return BackupArchive{}, errors.New("replication: unsupported backup format")
	}
	return readBindWSBackup(data)
}

func readBindWSBackup(data []byte) (BackupArchive, error) {
	var legacy struct {
		Format   string `json:"format"`
		Manifest struct {
			CreatedAt     int64  `json:"createdAt"`
			ArchiveSHA256 string `json:"archiveSha256"`
			Owner         string `json:"owner"`
			RelayIdentity string `json:"relayIdentity"`
			Slug          string `json:"slug"`
		} `json:"manifest"`
		Config json.RawMessage `json:"config"`
		Events []string        `json:"events"`
		Blobs  []struct {
			SHA256   string `json:"sha256"`
			Type     string `json:"type"`
			Uploader string `json:"uploader"`
			Uploaded int64  `json:"uploaded"`
			Data     string `json:"data"`
		} `json:"blobs"`
		Git []struct {
			Key    string `json:"key"`
			SHA256 string `json:"sha256"`
			Data   string `json:"data"`
		} `json:"git"`
		SQLGit json.RawMessage `json:"sqlGit"`
		State  json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return BackupArchive{}, err
	}
	if legacy.Manifest.ArchiveSHA256 == "" {
		return BackupArchive{}, errors.New("replication: legacy archive checksum missing")
	}
	canonical, err := blankLegacyArchiveHash(data)
	if err != nil || !strings.EqualFold(legacy.Manifest.ArchiveSHA256, digest(canonical)) {
		return BackupArchive{}, errors.New("replication: legacy archive checksum mismatch")
	}
	archive := BackupArchive{Format: BackupFormat, Created: legacy.Manifest.CreatedAt, State: BackupState{Config: legacy.Config, GitSQL: append(json.RawMessage(nil), legacy.SQLGit...)}}
	identity, _ := json.Marshal(map[string]string{"owner": legacy.Manifest.Owner, "relayIdentity": legacy.Manifest.RelayIdentity, "slug": legacy.Manifest.Slug})
	archive.State.Identity = identity
	if len(legacy.State) > 0 && string(legacy.State) != "null" {
		archive.State.Community = append(json.RawMessage(nil), legacy.State...)
	}
	for _, raw := range legacy.Events {
		var item event.Event
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			return BackupArchive{}, fmt.Errorf("replication: legacy event: %w", err)
		}
		if err := event.Validate(item); err != nil {
			return BackupArchive{}, fmt.Errorf("replication: legacy event validation: %w", err)
		}
		archive.Events = append(archive.Events, item)
	}
	for _, item := range legacy.Blobs {
		content, err := base64.StdEncoding.DecodeString(item.Data)
		if err != nil {
			return BackupArchive{}, err
		}
		if !validDigest(item.SHA256, content) {
			return BackupArchive{}, errors.New("replication: legacy blob checksum mismatch")
		}
		archive.State.Blobs = append(archive.State.Blobs, BackupObject{Name: item.SHA256, SHA256: item.SHA256, Content: content, Type: item.Type, Uploader: item.Uploader, Uploaded: item.Uploaded})
	}
	for _, item := range legacy.Git {
		content, err := base64.StdEncoding.DecodeString(item.Data)
		if err != nil {
			return BackupArchive{}, err
		}
		if item.SHA256 != "" && !validDigest(item.SHA256, content) {
			return BackupArchive{}, errors.New("replication: legacy git checksum mismatch")
		}
		archive.State.Git = append(archive.State.Git, BackupObject{Name: item.Key, SHA256: digest(content), Content: content})
	}
	return sealBackup(archive)
}

func blankLegacyArchiveHash(data []byte) ([]byte, error) {
	key := []byte(`"archiveSha256"`)
	start := bytes.Index(data, key)
	if start < 0 {
		return nil, errors.New("replication: legacy archive hash field missing")
	}
	colon := bytes.IndexByte(data[start+len(key):], ':')
	if colon < 0 {
		return nil, errors.New("replication: malformed legacy archive hash")
	}
	colon += start + len(key)
	quote := bytes.IndexByte(data[colon+1:], '"')
	if quote < 0 {
		return nil, errors.New("replication: malformed legacy archive hash")
	}
	quote += colon + 1
	end := bytes.IndexByte(data[quote+1:], '"')
	if end < 0 {
		return nil, errors.New("replication: malformed legacy archive hash")
	}
	end += quote + 1
	out := make([]byte, 0, len(data))
	out = append(out, data[:quote+1]...)
	out = append(out, data[end:]...)
	return out, nil
}

func validDigest(expected string, content []byte) bool {
	return expected != "" && strings.EqualFold(expected, digest(content))
}
func digest(content []byte) string { sum := sha256.Sum256(content); return hex.EncodeToString(sum[:]) }

func RestoreBackup(ctx context.Context, store *storage.Store, archive BackupArchive, now int64) (int, error) {
	return RestoreBackupWithProvider(ctx, store, nil, archive, now)
}

func RestoreBackupWithProvider(ctx context.Context, store *storage.Store, provider BackupProvider, archive BackupArchive, now int64) (int, error) {
	if archive.Format != BackupFormat {
		return 0, errors.New("replication: unsupported backup format")
	}
	sealed, err := sealBackup(BackupArchive{Format: archive.Format, Events: archive.Events, State: archive.State, Created: archive.Created})
	if err != nil {
		return 0, err
	}
	if sealed.SHA256 != archive.SHA256 {
		return 0, errors.New("replication: backup integrity check failed")
	}
	for _, item := range archive.Events {
		if err := event.Validate(item); err != nil {
			return 0, fmt.Errorf("replication: restore event validation: %w", err)
		}
	}
	if provider != nil && len(archive.State.Database) > 0 {
		previous, snapshotErr := provider.Snapshot(ctx)
		if snapshotErr != nil {
			return 0, fmt.Errorf("replication: snapshot retained state: %w", snapshotErr)
		}
		if err := provider.Restore(ctx, archive.State); err != nil {
			return 0, fmt.Errorf("replication: restore retained state: %w", err)
		}
		if finalizer, ok := provider.(RestoreFinalizer); ok {
			if err := finalizer.FinalizeRestore(ctx); err != nil {
				rollbackProvider(ctx, provider, previous)
				return 0, fmt.Errorf("replication: finalize restore: %w", err)
			}
		}
		return len(archive.Events), nil
	}
	var previous BackupState
	var havePrevious bool
	if provider != nil {
		previous, err = provider.Snapshot(ctx)
		if err != nil {
			return 0, fmt.Errorf("replication: snapshot retained state: %w", err)
		}
		havePrevious = true
		if err := provider.Restore(ctx, archive.State); err != nil {
			return 0, fmt.Errorf("replication: restore retained state: %w", err)
		}
	}
	stored := 0
	inserted := make([]string, 0, len(archive.Events))
	for _, item := range archive.Events {
		if err := ctx.Err(); err != nil {
			rollbackEvents(ctx, store, inserted)
			if havePrevious {
				rollbackProvider(ctx, provider, previous)
			}
			return stored, err
		}
		if _, err := store.Save(ctx, item, storage.SaveOptions{Now: now}); err != nil {
			if err == storage.ErrDuplicate || err == storage.ErrReplaced {
				continue
			}
			rollbackEvents(ctx, store, inserted)
			if havePrevious {
				rollbackProvider(ctx, provider, previous)
			}
			return stored, fmt.Errorf("replication: restore event: %w", err)
		}
		inserted = append(inserted, item.ID)
		stored++
	}
	if finalizer, ok := provider.(RestoreFinalizer); ok {
		if err := finalizer.FinalizeRestore(ctx); err != nil {
			rollbackEvents(ctx, store, inserted)
			if havePrevious {
				rollbackProvider(ctx, provider, previous)
			}
			return stored, fmt.Errorf("replication: finalize restore: %w", err)
		}
	}
	return stored, nil
}

func restoreRollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	return context.WithTimeout(base, 30*time.Second)
}

func rollbackEvents(ctx context.Context, store *storage.Store, ids []string) {
	rollback, cancel := restoreRollbackContext(ctx)
	defer cancel()
	rollbackRestoreEvents(rollback, store, ids)
}

func rollbackProvider(ctx context.Context, provider BackupProvider, state BackupState) {
	if provider == nil {
		return
	}
	rollback, cancel := restoreRollbackContext(ctx)
	defer cancel()
	_ = provider.Restore(rollback, state)
}

func rollbackRestoreEvents(ctx context.Context, store *storage.Store, ids []string) {
	if len(ids) == 0 || store == nil {
		return
	}
	_ = store.WithTx(ctx, func(tx *sql.Tx) error {
		marks := make([]string, len(ids))
		args := make([]any, len(ids))
		for i, id := range ids {
			marks[i], args[i] = "?", id
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (`+strings.Join(marks, ",")+`)`, args...)
		return err
	})
}

func sealBackup(archive BackupArchive) (BackupArchive, error) {
	archive.SHA256 = ""
	raw, err := json.Marshal(archive)
	if err != nil {
		return archive, err
	}
	sum := sha256.Sum256(raw)
	archive.SHA256 = hex.EncodeToString(sum[:])
	return archive, nil
}
