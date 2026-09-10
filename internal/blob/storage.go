package blob

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// Put stores bytes using their SHA-256 digest as the durable filename.
func (s *Service) Put(ctx context.Context, options PutOptions) (Blob, error) {
	if options.Reader == nil {
		return Blob{}, errors.New("blob reader is required")
	}
	entry, _, err := s.putOptions(ctx, options)
	return entry, err
}

func (s *Service) putOptions(ctx context.Context, options PutOptions) (Blob, bool, error) {
	return s.putValidatedWithMetadata(ctx, options.Reader, options.Type, options.Uploader, options.Hash, options.Name, options.Path, nil, options.Commit)
}

// Get opens a verified metadata-backed blob for streaming to another service.
func (s *Service) Get(ctx context.Context, sha string) (Blob, io.ReadCloser, error) {
	if !shaPattern.MatchString(sha) {
		return Blob{}, nil, errors.New("invalid: bad blob sha256")
	}
	entry, err := s.lookup(ctx, sha)
	if err != nil {
		return Blob{}, nil, err
	}
	if s.blocked(ctx, sha) {
		return Blob{}, nil, ErrBlocked
	}
	file, err := os.Open(filepath.Join(s.root, sha))
	if err != nil {
		return Blob{}, nil, err
	}
	return entry, file, nil
}

// ListMetadata returns all blobs uploaded by the supplied pubkey.
func (s *Service) ListMetadata(ctx context.Context, uploader string) ([]Blob, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT b.sha256,b.size,b.type,c.uploader,b.uploaded
		FROM blobs b JOIN blob_claims c ON c.sha256=b.sha256 WHERE c.uploader=?
		ORDER BY b.uploaded DESC,b.sha256 DESC`, uploader)
	if err != nil {
		return nil, fmt.Errorf("list blob metadata: %w", err)
	}
	defer rows.Close()
	entries := make([]Blob, 0)
	for rows.Next() {
		var entry Blob
		if err := rows.Scan(&entry.SHA256, &entry.Size, &entry.Type, &entry.Uploader, &entry.Uploaded); err != nil {
			return nil, fmt.Errorf("scan blob metadata: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// ListPage returns a BUD-12 cursor page. The cursor is the last blob hash
// returned by the previous page and is intentionally opaque to callers.
func (s *Service) ListPage(ctx context.Context, uploader string, limit int, cursor string) ([]Blob, string, error) {
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	query := `SELECT b.sha256,b.size,b.type,c.uploader,b.uploaded FROM blobs b
		JOIN blob_claims c ON c.sha256=b.sha256 WHERE c.uploader=?`
	args := []any{uploader}
	if cursor != "" {
		var uploaded int64
		if err := s.store.DB().QueryRowContext(ctx, `SELECT b.uploaded FROM blobs b
			JOIN blob_claims c ON c.sha256=b.sha256 WHERE b.sha256=? AND c.uploader=?`, cursor, uploader).Scan(&uploaded); err != nil {
			return nil, "", fmt.Errorf("resolve blob cursor: %w", err)
		}
		query += " AND (b.uploaded < ? OR (b.uploaded = ? AND b.sha256 < ?))"
		args = append(args, uploaded, uploaded, cursor)
	}
	query += " ORDER BY b.uploaded DESC,b.sha256 DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list blob page: %w", err)
	}
	defer rows.Close()
	entries := make([]Blob, 0, limit)
	for rows.Next() {
		var entry Blob
		if err := rows.Scan(&entry.SHA256, &entry.Size, &entry.Type, &entry.Uploader, &entry.Uploaded); err != nil {
			return nil, "", fmt.Errorf("scan blob page: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("read blob page: %w", err)
	}
	next := ""
	if len(entries) > limit {
		next = entries[limit-1].SHA256
		entries = entries[:limit]
	}
	return entries, next, nil
}

// Delete removes a blob's metadata and content. Authorization is the caller's responsibility.
func (s *Service) Delete(ctx context.Context, sha string) error {
	if !shaPattern.MatchString(sha) {
		return errors.New("invalid: bad blob sha256")
	}
	if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO blob_tombstones(sha256,deleted_at) VALUES(?,?) ON CONFLICT(sha256) DO UPDATE SET deleted_at=excluded.deleted_at", sha, time.Now().UTC().Unix()); err != nil {
			return fmt.Errorf("record blob deletion: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM blobs WHERE sha256=?", sha); err != nil {
			return fmt.Errorf("delete blob metadata: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM blob_claim_metadata WHERE sha256=?", sha); err != nil {
			return fmt.Errorf("delete blob claim metadata: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.root, sha)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete blob content: %w", err)
	}
	return nil
}

// DeleteForUploader removes one owner's claim. The physical object remains
// while another owner still references it, which makes deduplicated uploads
// safe to manage independently.
func (s *Service) DeleteForUploader(ctx context.Context, sha, uploader string) error {
	if !shaPattern.MatchString(sha) {
		return errors.New("invalid: bad blob sha256")
	}
	removed := false
	if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "DELETE FROM blob_claims WHERE sha256=? AND uploader=?", sha, uploader)
		if err != nil {
			return fmt.Errorf("delete blob claim: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check blob claim deletion: %w", err)
		}
		if changed == 0 {
			return sql.ErrNoRows
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM blob_claim_metadata WHERE sha256=? AND uploader=?", sha, uploader); err != nil {
			return fmt.Errorf("delete blob claim metadata: %w", err)
		}
		var remaining int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM blob_claims WHERE sha256=?", sha).Scan(&remaining); err != nil {
			return fmt.Errorf("count blob claims: %w", err)
		}
		if remaining == 0 {
			removed = true
			if _, err := tx.ExecContext(ctx, "INSERT INTO blob_tombstones(sha256,deleted_at) VALUES(?,?) ON CONFLICT(sha256) DO UPDATE SET deleted_at=excluded.deleted_at", sha, time.Now().UTC().Unix()); err != nil {
				return fmt.Errorf("record blob deletion: %w", err)
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM blobs WHERE sha256=?", sha); err != nil {
				return fmt.Errorf("delete blob metadata: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if removed {
		if err := os.Remove(filepath.Join(s.root, sha)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete blob content: %w", err)
		}
	}
	return nil
}

// Descriptor returns the Blossom descriptor and NIP-94 tags for a blob.
func (s *Service) Descriptor(baseURL string, entry Blob) map[string]any {
	return map[string]any{
		"url":      baseURL + "/" + entry.SHA256 + extensionSuffix(entry.Type),
		"sha256":   entry.SHA256,
		"size":     entry.Size,
		"type":     entry.Type,
		"uploaded": entry.Uploaded,
		"nip94":    [][]string{{"url", baseURL + "/" + entry.SHA256 + extensionSuffix(entry.Type)}, {"m", entry.Type}, {"x", entry.SHA256}, {"ox", entry.SHA256}, {"size", strconv.FormatInt(entry.Size, 10)}},
	}
}
func (s *Service) put(ctx context.Context, source io.Reader, typ, uploader, claimed string) (Blob, bool, error) {
	return s.putValidatedWithCommit(ctx, source, typ, uploader, claimed, nil, nil)
}

func (s *Service) putValidated(ctx context.Context, source io.Reader, typ, uploader, claimed string, validate func(string) error) (Blob, bool, error) {
	return s.putValidatedWithMetadata(ctx, source, typ, uploader, claimed, "", "", validate, nil)
}

func (s *Service) putValidatedWithCommit(ctx context.Context, source io.Reader, typ, uploader, claimed string, validate func(string) error, commit func(context.Context, *sql.Tx, Blob, bool) error) (Blob, bool, error) {
	return s.putValidatedWithMetadata(ctx, source, typ, uploader, claimed, "", "", validate, commit)
}

func (s *Service) putValidatedWithMetadata(ctx context.Context, source io.Reader, typ, uploader, claimed, name, path string, validate func(string) error, commit func(context.Context, *sql.Tx, Blob, bool) error) (Blob, bool, error) {
	var err error
	name, path, err = claimMetadata(name, path)
	if err != nil {
		return Blob{}, false, err
	}
	limits := s.currentLimits()
	streamLimit := limits.MaxFileBytes
	quotaStream := false
	if streamLimit == 0 && limits.UserStorageBytes > 0 && uploader != "" {
		streamLimit = limits.UserStorageBytes
		quotaStream = true
	}
	if streamLimit > 0 && streamLimit < int64(^uint64(0)>>1) {
		source = io.LimitReader(source, streamLimit+1)
	}
	tmp, err := os.CreateTemp(s.root, ".upload-")
	if err != nil {
		return Blob{}, false, fmt.Errorf("create upload temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hasher := sha256.New()
	count, err := io.Copy(io.MultiWriter(tmp, hasher), source)
	if err != nil {
		_ = tmp.Close()
		return Blob{}, false, fmt.Errorf("read upload: %w", err)
	}
	if limits.MaxFileBytes > 0 && count > limits.MaxFileBytes {
		_ = tmp.Close()
		return Blob{}, false, ErrFileTooLarge
	}
	if quotaStream && count > streamLimit {
		_ = tmp.Close()
		return Blob{}, false, ErrQuotaExceeded
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Blob{}, false, fmt.Errorf("sync upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Blob{}, false, fmt.Errorf("close upload: %w", err)
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	if claimed != "" && claimed != sha {
		return Blob{}, false, ErrHashMismatch
	}
	if validate != nil {
		if err := validate(sha); err != nil {
			return Blob{}, false, err
		}
	}
	return s.installUpload(ctx, uploadCandidate{path: tmpName, hash: sha, size: count, typ: typ, uploader: uploader, name: name, metadataPath: path, commit: commit})
}

type uploadCandidate struct {
	path, hash, typ, uploader, reservation, name, metadataPath string
	size                                                       int64
	commit                                                     func(context.Context, *sql.Tx, Blob, bool) error
}

func (s *Service) installUpload(ctx context.Context, candidate uploadCandidate) (Blob, bool, error) {
	sha, count, typ, uploader := candidate.hash, candidate.size, candidate.typ, candidate.uploader
	// Serialize the lookup, quota check, installation and metadata transaction.
	// This closes the race where concurrent uploads could each observe the same
	// remaining quota before one of them installs a new physical object.
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	if s.blocked(ctx, sha) {
		return Blob{}, false, ErrBlocked
	}
	terms, err := s.uploadTerms(ctx, uploader)
	if err != nil {
		return Blob{}, false, err
	}
	limits := s.currentLimits()
	if limits.MaxFileBytes > 0 && count > limits.MaxFileBytes {
		return Blob{}, false, ErrFileTooLarge
	}
	if limits.UserStorageBytes > 0 && uploader != "" && count > limits.UserStorageBytes {
		return Blob{}, false, ErrQuotaExceeded
	}
	if existing, err := s.lookup(ctx, sha); err == nil {
		if terms.Encrypted && !encryptedTypes[existing.Type] {
			return Blob{}, false, ErrNotEncrypted
		}
		if uploader != "" {
			if limits.UserStorageBytes > 0 && !s.hasClaim(ctx, sha, uploader) {
				used, usageErr := s.quotaUsageExcept(ctx, uploader, candidate.reservation)
				if usageErr != nil {
					return Blob{}, false, fmt.Errorf("check uploader storage quota: %w", usageErr)
				}
				if existing.Size > limits.UserStorageBytes-used {
					return Blob{}, false, ErrQuotaExceeded
				}
			}
		}
		if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
			if candidate.commit != nil {
				if err := candidate.commit(ctx, tx, existing, false); err != nil {
					return err
				}
			}
			if uploader != "" {
				if _, err := tx.ExecContext(ctx, claimSQL, sha, uploader, time.Now().Unix(), terms.ExpiresAt); err != nil {
					return err
				}
				if err := s.saveClaimMetadata(ctx, tx, sha, uploader, candidate.name, candidate.metadataPath); err != nil {
					return err
				}
			}
			if candidate.reservation != "" {
				if _, err := tx.ExecContext(ctx, "DELETE FROM multipart_uploads WHERE id=?", candidate.reservation); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return Blob{}, false, err
		}
		// A crash after a scoped metadata commit may leave the bytes missing.
		// A retry supplies those same verified bytes without losing the scope.
		if _, err := os.Stat(filepath.Join(s.root, sha)); errors.Is(err, os.ErrNotExist) {
			if err := s.installBlobFile(candidate.path, sha); err != nil {
				return Blob{}, false, err
			}
		} else if err != nil {
			return Blob{}, false, err
		}
		return existing, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Blob{}, false, err
	}
	if limits.UserStorageBytes > 0 && uploader != "" {
		used, err := s.quotaUsageExcept(ctx, uploader, candidate.reservation)
		if err != nil {
			return Blob{}, false, fmt.Errorf("check uploader storage quota: %w", err)
		}
		if count > limits.UserStorageBytes-used {
			return Blob{}, false, ErrQuotaExceeded
		}
	}
	// The type is settled before the bytes are installed so a plain upload
	// under an encrypted-only grant leaves nothing behind. A declared octet
	// stream is sniffed: encrypted bytes stay one, a page or an image does not.
	typ = contentType(typ)
	if typ == "application/octet-stream" {
		typ = detectFileType(candidate.path)
	}
	if terms.Encrypted && !encryptedTypes[typ] {
		return Blob{}, false, ErrNotEncrypted
	}
	stage := ""
	if candidate.commit != nil {
		stage = filepath.Join(s.root, ".scoped-"+sha)
		if err := os.Rename(candidate.path, stage); err != nil {
			return Blob{}, false, fmt.Errorf("stage scoped blob: %w", err)
		}
		if err := syncDirectory(s.root); err != nil {
			return Blob{}, false, fmt.Errorf("sync staged blob directory: %w", err)
		}
	}
	if candidate.commit == nil {
		if err := s.installBlobFile(candidate.path, sha); err != nil {
			return Blob{}, false, err
		}
	}
	now := time.Now().UTC().Unix()
	entry := Blob{SHA256: sha, Size: count, Type: typ, Uploader: uploader, Uploaded: now}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM blob_tombstones WHERE sha256=?", entry.SHA256); err != nil {
			return fmt.Errorf("clear blob deletion tombstone: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO blobs(sha256,size,type,uploader,uploaded) VALUES(?,?,?,?,?)", entry.SHA256, entry.Size, entry.Type, entry.Uploader, entry.Uploaded); err != nil {
			return fmt.Errorf("record blob: %w", err)
		}
		if entry.Uploader != "" {
			if _, err := tx.ExecContext(ctx, claimSQL, entry.SHA256, entry.Uploader, entry.Uploaded, terms.ExpiresAt); err != nil {
				return fmt.Errorf("record blob claim: %w", err)
			}
			if err := s.saveClaimMetadata(ctx, tx, entry.SHA256, entry.Uploader, candidate.name, candidate.metadataPath); err != nil {
				return err
			}
		}
		if candidate.reservation != "" {
			if _, err := tx.ExecContext(ctx, "DELETE FROM multipart_uploads WHERE id=?", candidate.reservation); err != nil {
				return fmt.Errorf("release upload reservation: %w", err)
			}
		}
		if candidate.commit != nil {
			if err := candidate.commit(ctx, tx, entry, true); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if stage != "" {
			_ = os.Remove(stage)
		}
		return Blob{}, false, err
	}
	if candidate.commit != nil {
		if err := s.installBlobFile(stage, sha); err != nil {
			return Blob{}, false, err
		}
	}
	if saved, err := s.lookup(ctx, sha); err == nil {
		return saved, saved.Uploader == uploader && saved.Uploaded == now, nil
	}
	return entry, true, nil
}

func (s *Service) installBlobFile(source, hash string) error {
	if err := os.Rename(source, filepath.Join(s.root, hash)); err != nil {
		return fmt.Errorf("install blob: %w", err)
	}
	if err := syncDirectory(s.root); err != nil {
		return fmt.Errorf("sync blob directory: %w", err)
	}
	return nil
}

func (s *Service) quotaUsage(ctx context.Context, uploader string) (int64, error) {
	return s.quotaUsageExcept(ctx, uploader, "")
}

func (s *Service) quotaUsageExcept(ctx context.Context, uploader, reservation string) (int64, error) {
	var used sql.NullInt64
	err := s.store.DB().QueryRowContext(ctx, `SELECT COALESCE((SELECT SUM(b.size)
		FROM blobs b JOIN blob_claims c ON c.sha256=b.sha256 WHERE c.uploader=?),0) +
		COALESCE((SELECT SUM(length) FROM multipart_uploads WHERE uploader=? AND id != ?),0)`, uploader, uploader, reservation).Scan(&used)
	if err != nil {
		return 0, err
	}
	return used.Int64, nil
}

// claimSQL records a claim. The insert trigger on blobs has usually made the
// row already, without an expiry, so the newest upload's terms replace it.
const claimSQL = "INSERT INTO blob_claims(sha256,uploader,claimed_at,expires) VALUES(?,?,?,?) ON CONFLICT(sha256,uploader) DO UPDATE SET expires=excluded.expires"

func (s *Service) claim(ctx context.Context, sha, uploader string, expires int64) error {
	_, err := s.store.DB().ExecContext(ctx, claimSQL, sha, uploader, time.Now().UTC().Unix(), expires)
	if err != nil {
		return fmt.Errorf("record blob claim: %w", err)
	}
	return nil
}

func (s *Service) uploadTerms(ctx context.Context, uploader string) (UploadTerms, error) {
	if s.config.UploadTerms == nil || uploader == "" {
		return UploadTerms{}, nil
	}
	// A refusal keeps its reason so the door answers with the right status.
	return s.config.UploadTerms(ctx, uploader)
}

// SweepExpired removes the blobs whose every claim has lapsed and releases
// lapsed claims on blobs someone else still holds, so an agent's files leave
// with its site while a person's claim keeps them. It returns how many blobs
// were removed.
func (s *Service) SweepExpired(ctx context.Context, now int64) (int, error) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	rows, err := s.store.DB().QueryContext(ctx, `SELECT DISTINCT sha256 FROM blob_claims c WHERE c.expires>0 AND c.expires<=?
		AND NOT EXISTS(SELECT 1 FROM blob_claims live WHERE live.sha256=c.sha256 AND (live.expires=0 OR live.expires>?))`, now, now)
	if err != nil {
		return 0, fmt.Errorf("list expired blobs: %w", err)
	}
	var expired []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			_ = rows.Close()
			return 0, err
		}
		expired = append(expired, sha)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	for _, sha := range expired {
		if err := s.Delete(ctx, sha); err != nil {
			return 0, err
		}
	}
	if _, err := s.store.DB().ExecContext(ctx, "DELETE FROM blob_claims WHERE expires>0 AND expires<=?", now); err != nil {
		return 0, fmt.Errorf("release expired blob claims: %w", err)
	}
	return len(expired), nil
}

func (s *Service) currentLimits() Limits {
	if s.config.Limits == nil {
		return Limits{}
	}
	limits := s.config.Limits()
	if limits.MaxFileBytes < 0 {
		limits.MaxFileBytes = 0
	}
	if limits.UserStorageBytes < 0 {
		limits.UserStorageBytes = 0
	}
	return limits
}

func (s *Service) hasClaim(ctx context.Context, sha, uploader string) bool {
	var found int
	err := s.store.DB().QueryRowContext(ctx, "SELECT 1 FROM blob_claims WHERE sha256=? AND uploader=?", sha, uploader).Scan(&found)
	return err == nil && found == 1
}

func (s *Service) lookup(ctx context.Context, sha string) (Blob, error) {
	var entry Blob
	err := s.store.DB().QueryRowContext(ctx, "SELECT sha256,size,type,uploader,uploaded FROM blobs WHERE sha256=?", sha).Scan(&entry.SHA256, &entry.Size, &entry.Type, &entry.Uploader, &entry.Uploaded)
	return entry, err
}

func (s *Service) blocked(ctx context.Context, sha string) bool {
	var found int
	err := s.store.DB().QueryRowContext(ctx, `SELECT 1 FROM hidden_events WHERE id=?
		UNION ALL SELECT 1 FROM blob_blocks WHERE sha256=? LIMIT 1`, sha, sha).Scan(&found)
	return err == nil && found == 1
}

func reconcile(ctx context.Context, store *storage.Store, root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("scan blob directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".scoped-") {
			sha := strings.TrimPrefix(name, ".scoped-")
			if !shaPattern.MatchString(sha) {
				if err := os.Remove(filepath.Join(root, name)); err != nil {
					return fmt.Errorf("remove invalid staged blob: %w", err)
				}
				continue
			}
			if err := reconcileStaged(ctx, store, root, name, sha); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(name, ".upload-") || strings.HasPrefix(name, ".multipart-chunk-") {
			if err := os.Remove(filepath.Join(root, name)); err != nil {
				return fmt.Errorf("remove abandoned upload: %w", err)
			}
			continue
		}
		if !shaPattern.MatchString(name) {
			continue
		}
		var deleted int
		if err := store.DB().QueryRowContext(ctx, "SELECT 1 FROM blob_tombstones WHERE sha256=? UNION ALL SELECT 1 FROM blob_blocks WHERE sha256=? LIMIT 1", name, name).Scan(&deleted); err == nil {
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check deleted blob %q: %w", name, err)
		}
		var known int
		if err := store.DB().QueryRowContext(ctx, "SELECT 1 FROM blobs WHERE sha256=?", name).Scan(&known); err == nil {
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check orphan blob %q: %w", name, err)
		}
		if err := recoverBlob(ctx, store, root, name); err != nil {
			return err
		}
	}
	return nil
}

func reconcileStaged(ctx context.Context, store *storage.Store, root, name, sha string) error {
	var size int64
	if err := store.DB().QueryRowContext(ctx, "SELECT size FROM blobs WHERE sha256=?", sha).Scan(&size); errors.Is(err, sql.ErrNoRows) {
		if removeErr := os.Remove(filepath.Join(root, name)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("check staged blob %q: %w", sha, err)
	}
	file, err := os.Open(filepath.Join(root, name))
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if info.Size() != size {
		_ = file.Close()
		return fmt.Errorf("staged blob %q has size %d, want %d", sha, info.Size(), size)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if hex.EncodeToString(hasher.Sum(nil)) != sha {
		return fmt.Errorf("staged blob %q failed content hash", sha)
	}
	if _, err := os.Stat(filepath.Join(root, sha)); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(filepath.Join(root, name), filepath.Join(root, sha)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		_ = os.Remove(filepath.Join(root, name))
	}
	return syncDirectory(root)
}

func recoverBlob(ctx context.Context, store *storage.Store, root, name string) error {
	file, err := os.Open(filepath.Join(root, name))
	if err != nil {
		return fmt.Errorf("open orphan blob %q: %w", name, err)
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("read orphan blob %q: %w", name, errors.Join(copyErr, closeErr))
	}
	if hex.EncodeToString(hash.Sum(nil)) != name {
		return fmt.Errorf("orphan blob %q failed content hash", name)
	}
	info, err := os.Stat(filepath.Join(root, name))
	if err != nil {
		return fmt.Errorf("stat orphan blob %q: %w", name, err)
	}
	_, err = store.DB().ExecContext(ctx, "INSERT OR IGNORE INTO blobs(sha256,size,type,uploader,uploaded) VALUES(?,?,?,?,?)", name, size, "application/octet-stream", "", info.ModTime().Unix())
	if err != nil {
		return fmt.Errorf("recover orphan blob %q: %w", name, err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
