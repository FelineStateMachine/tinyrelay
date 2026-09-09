// Package blob implements the Blossom and NIP-96 file doors for a local relay.
package blob

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type Action string

const (
	ActionUpload Action = "upload"
	ActionGet    Action = "get"
	ActionDelete Action = "delete"
	ActionList   Action = "list"
	ActionMirror Action = "mirror"
	ActionReport Action = "report"
)

type Authorize func(*http.Request, Action) (string, error)
type CanRead func(context.Context, string, []string) bool
type ResolveIP func(context.Context, string) ([]net.IP, error)
type ReportRecorder func(context.Context, *sql.Tx, string, string, string, string, string) error

type Config struct {
	Root           string
	PublicURL      string
	Store          *storage.Store
	Authorize      Authorize
	CanRead        CanRead
	HTTPClient     *http.Client
	ResolveIP      ResolveIP
	IsOwner        func(string) bool
	ValidateUpload func(*http.Request, string) error
	// ResolveServers returns the ordered BUD-03 servers for an authorized
	// caller. It is used only for Blossom URI mirror fallback.
	ResolveServers func(context.Context, string) ([]string, error)
	// RecordReport joins native blob reporting to the host moderation ledger.
	// It runs inside the blob report transaction.
	RecordReport ReportRecorder
	// Limits is read for each upload so policy changes apply without reopening
	// the blob service. Zero values mean unlimited.
	Limits func() Limits
}

type Limits struct {
	MaxFileBytes     int64
	UserStorageBytes int64
}

type Blob struct {
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Type     string `json:"type"`
	Uploader string `json:"uploader"`
	Uploaded int64  `json:"uploaded"`
}

type PutOptions struct {
	Reader   io.Reader
	Type     string
	Uploader string
	Hash     string
}

type Service struct {
	config  Config
	root    string
	store   *storage.Store
	resolve ResolveIP
	quotaMu sync.Mutex
	// multipartMu protects partial files and their range metadata. Lock it
	// before quotaMu; network bodies are never read while either is held.
	multipartMu sync.Mutex
	activeParts map[string]int
	// multipartFinalizing prevents another chunk from changing a complete
	// session while its assembled file is being hashed. Hashing is deliberately
	// outside multipartMu because the upload may be as large as 1 GiB.
	multipartFinalizing map[string]bool
	// verifyMultipartHook is used by package tests to pause finalization
	// without allocating a GiB fixture. It is nil in production.
	verifyMultipartHook func(context.Context, string, string, int64) error
}

var ErrBlocked = errors.New("blocked: this blob was removed by a moderator")
var ErrHashMismatch = errors.New("conflict: mirrored blob hash does not match the requested hash")
var ErrFileTooLarge = errors.New("invalid: blob exceeds the per-file size limit")
var ErrQuotaExceeded = errors.New("restricted: uploader storage quota exceeded")
var ErrMultipartConflict = errors.New("conflict: upload metadata differs from existing session")

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var blobPathPattern = regexp.MustCompile(`^/([0-9a-f]{64})(?:\.[a-z0-9]{1,8})?$`)

// HandlesPath keeps the daemon mount in sync with every supported file door.
func HandlesPath(path string) bool {
	return path == "/upload" || path == "/mirror" || path == "/report" ||
		path == "/nip96" || path == "/.well-known/nostr/nip96.json" ||
		strings.HasPrefix(path, "/nip96/") || strings.HasPrefix(path, "/list/") ||
		blobPathPattern.MatchString(path)
}

var extensions = map[string]string{
	"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp", "image/avif": "avif", "image/svg+xml": "svg",
	"video/mp4": "mp4", "video/webm": "webm", "video/quicktime": "mov", "audio/mpeg": "mp3", "audio/ogg": "ogg", "audio/wav": "wav", "audio/mp4": "m4a",
	"application/pdf": "pdf", "application/json": "json", "text/plain": "txt", "text/markdown": "md",
	"application/vnd.blossom.directory+msgpack": "bdir",
}

func New(ctx context.Context, config Config) (*Service, error) {
	if config.Root == "" || config.Store == nil {
		return nil, errors.New("blob root and store are required")
	}
	if err := os.MkdirAll(filepath.Join(config.Root, "blobs"), 0o700); err != nil {
		return nil, fmt.Errorf("create blob root: %w", err)
	}
	if _, err := config.Store.DB().ExecContext(ctx, `CREATE TABLE IF NOT EXISTS blobs (
		sha256 TEXT PRIMARY KEY, size INTEGER NOT NULL, type TEXT NOT NULL,
		uploader TEXT NOT NULL, uploaded INTEGER NOT NULL
	); CREATE TABLE IF NOT EXISTS blob_claims (
		sha256 TEXT NOT NULL, uploader TEXT NOT NULL, claimed_at INTEGER NOT NULL,
		PRIMARY KEY (sha256, uploader), FOREIGN KEY (sha256) REFERENCES blobs(sha256) ON DELETE CASCADE
	); CREATE INDEX IF NOT EXISTS blobs_uploader ON blobs(uploader, uploaded DESC);
	CREATE INDEX IF NOT EXISTS blob_claims_uploader ON blob_claims(uploader, claimed_at DESC, sha256 DESC);
	CREATE TABLE IF NOT EXISTS blob_reports (
	 id TEXT PRIMARY KEY, reporter TEXT NOT NULL, target_pubkey TEXT NOT NULL DEFAULT '',
	 target_blob TEXT NOT NULL, type TEXT NOT NULL, content TEXT NOT NULL, at INTEGER NOT NULL,
	 status TEXT NOT NULL DEFAULT 'open', resolved_by TEXT NOT NULL DEFAULT '',
	 resolved_at INTEGER NOT NULL DEFAULT 0, action TEXT NOT NULL DEFAULT '',
	 UNIQUE(reporter,target_blob));
	CREATE TRIGGER IF NOT EXISTS blob_claim_on_insert AFTER INSERT ON blobs
		WHEN NEW.uploader != '' BEGIN INSERT OR IGNORE INTO blob_claims(sha256,uploader,claimed_at) VALUES(NEW.sha256,NEW.uploader,NEW.uploaded); END;
	CREATE TABLE IF NOT EXISTS blob_blocks (sha256 TEXT PRIMARY KEY, reason TEXT NOT NULL DEFAULT '', blocked_at INTEGER NOT NULL DEFAULT 0);
	CREATE TABLE IF NOT EXISTS blob_tombstones (sha256 TEXT PRIMARY KEY, deleted_at INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS multipart_uploads (id TEXT PRIMARY KEY, sha256 TEXT NOT NULL, uploader TEXT NOT NULL, length INTEGER NOT NULL, type TEXT NOT NULL, created INTEGER NOT NULL, last_seen INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS multipart_parts (upload_id TEXT NOT NULL, offset INTEGER NOT NULL, length INTEGER NOT NULL, PRIMARY KEY(upload_id, offset), FOREIGN KEY(upload_id) REFERENCES multipart_uploads(id) ON DELETE CASCADE);
	INSERT OR IGNORE INTO blob_claims(sha256,uploader,claimed_at)
		SELECT sha256,uploader,uploaded FROM blobs WHERE uploader != ''`); err != nil {
		return nil, fmt.Errorf("initialize blob metadata: %w", err)
	}
	if err := reconcile(ctx, config.Store, filepath.Join(config.Root, "blobs")); err != nil {
		return nil, err
	}
	resolve := config.ResolveIP
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	s := &Service{config: config, root: filepath.Join(config.Root, "blobs"), store: config.Store, resolve: resolve, activeParts: make(map[string]int), multipartFinalizing: make(map[string]bool)}
	if err := s.reconcileMultipart(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) Handler() http.HandlerFunc { return s.serveHTTP }

// Put stores bytes using their SHA-256 digest as the durable filename.
func (s *Service) Put(ctx context.Context, options PutOptions) (Blob, error) {
	if options.Reader == nil {
		return Blob{}, errors.New("blob reader is required")
	}
	entry, _, err := s.put(ctx, options.Reader, options.Type, options.Uploader, options.Hash)
	return entry, err
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

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("access-control-allow-origin", "*")
	w.Header().Set("access-control-allow-headers", "authorization, content-type, x-sha-256, x-content-length, x-content-type, upload-type, upload-length, upload-offset")
	w.Header().Set("access-control-allow-methods", "GET, HEAD, PUT, PATCH, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Expose-Headers", "Allow, X-Reason")
	if r.Method == http.MethodOptions {
		if blobPathPattern.MatchString(r.URL.Path) {
			w.Header().Set("Allow", "GET, HEAD, PUT, PATCH, DELETE, OPTIONS")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/.well-known/nostr/nip96.json" {
		s.writeJSON(w, http.StatusOK, s.nip96Info(r))
		return
	}
	if r.URL.Path == "/upload" && r.Method == http.MethodHead {
		s.uploadHead(w, r)
		return
	}
	if r.URL.Path == "/upload" && r.Method == http.MethodPut {
		s.upload(w, r)
		return
	}
	if r.URL.Path == "/mirror" && r.Method == http.MethodPut {
		s.mirror(w, r)
		return
	}
	if match := blobPathPattern.FindStringSubmatch(r.URL.Path); match != nil && (r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodOptions) {
		if r.Method == http.MethodPatch || r.Method == http.MethodOptions {
			s.multipart(w, r, match[1])
			return
		}
		s.pathUpload(w, r, match[1])
		return
	}
	if r.URL.Path == "/report" && r.Method == http.MethodPut {
		s.report(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/list/") && r.Method == http.MethodGet {
		s.list(w, r, strings.TrimPrefix(r.URL.Path, "/list/"))
		return
	}
	if r.URL.Path == "/nip96" {
		s.nip96(w, r)
		return
	}
	s.blob(w, r)
}

func (s *Service) upload(w http.ResponseWriter, r *http.Request) {
	pubkey, err := s.authorize(r, ActionUpload)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, err)
		return
	}
	claimed := strings.ToLower(strings.TrimSpace(r.Header.Get("x-sha-256")))
	if claimed != "" && !shaPattern.MatchString(claimed) {
		s.fail(w, http.StatusBadRequest, errors.New("invalid: X-SHA-256 must be lowercase hex"))
		return
	}
	blob, created, err := s.putValidated(r.Context(), r.Body, contentType(r.Header.Get("content-type")), pubkey, claimed, func(actual string) error {
		if s.config.ValidateUpload != nil {
			return s.config.ValidateUpload(r, actual)
		}
		return nil
	})
	if err != nil {
		s.fail(w, statusFor(err), err)
		return
	}
	_ = created
	s.writeJSON(w, http.StatusOK, s.descriptor(r, blob))
}

func (s *Service) uploadHead(w http.ResponseWriter, r *http.Request) {
	sha := strings.ToLower(strings.TrimSpace(r.Header.Get("x-sha-256")))
	if !shaPattern.MatchString(sha) {
		s.fail(w, http.StatusBadRequest, errors.New("invalid: X-SHA-256 must be lowercase hex"))
		return
	}
	rawLength := strings.TrimSpace(r.Header.Get("x-content-length"))
	if rawLength == "" {
		s.fail(w, http.StatusLengthRequired, errors.New("invalid: X-Content-Length is required"))
		return
	}
	length, err := strconv.ParseInt(rawLength, 10, 64)
	if err != nil || length < 0 {
		s.fail(w, http.StatusBadRequest, errors.New("invalid: X-Content-Length must be a nonnegative integer"))
		return
	}
	pubkey, err := s.authorize(r, ActionUpload)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, err)
		return
	}
	typ := r.Header.Get("x-content-type")
	if typ == "" {
		typ = r.Header.Get("content-type")
	}
	if strings.TrimSpace(typ) == "" {
		s.fail(w, http.StatusBadRequest, errors.New("invalid: X-Content-Type is required"))
		return
	}
	if existing, lookupErr := s.lookup(r.Context(), sha); lookupErr == nil && existing.SHA256 != "" && s.hasClaim(r.Context(), sha, pubkey) {
		w.WriteHeader(http.StatusOK)
		return
	}
	limits := s.currentLimits()
	if limits.MaxFileBytes > 0 && length > limits.MaxFileBytes {
		s.fail(w, http.StatusRequestEntityTooLarge, ErrFileTooLarge)
		return
	}
	// HEAD is advisory for deduplicated uploads, whose final physical size is
	// already present, but it should still reflect the current quota budget.
	if limits.UserStorageBytes > 0 {
		if used, quotaErr := s.quotaUsage(r.Context(), pubkey); quotaErr != nil {
			s.fail(w, http.StatusInternalServerError, quotaErr)
			return
		} else if length > limits.UserStorageBytes-used {
			s.fail(w, http.StatusForbidden, ErrQuotaExceeded)
			return
		}
	}
	if existing, err := s.lookup(r.Context(), sha); err == nil && existing.SHA256 != "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Service) blob(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/nip96/") {
		path = strings.TrimPrefix(path, "/nip96")
	}
	match := blobPathPattern.FindStringSubmatch(path)
	if match == nil {
		s.fail(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	sha := match[1]
	if r.Method == http.MethodDelete {
		s.delete(w, r, sha)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if s.config.CanRead != nil {
		pubkey, err := s.authorize(r, ActionGet)
		if err != nil {
			s.fail(w, http.StatusUnauthorized, err)
			return
		}
		if !s.config.CanRead(r.Context(), sha, []string{pubkey}) {
			s.fail(w, http.StatusForbidden, errors.New("restricted: read access denied"))
			return
		}
	}
	entry, err := s.lookup(r.Context(), sha)
	if errors.Is(err, sql.ErrNoRows) {
		s.fail(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if s.blocked(r.Context(), sha) {
		s.fail(w, http.StatusForbidden, ErrBlocked)
		return
	}
	file, err := os.Open(filepath.Join(s.root, sha))
	if err != nil {
		s.fail(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	defer file.Close()
	w.Header().Set("etag", `"`+sha+`"`)
	w.Header().Set("Content-Type", entry.Type)
	w.Header().Set("x-content-type-options", "nosniff")
	w.Header().Set("accept-ranges", "bytes")
	w.Header().Set("content-security-policy", "default-src 'none'")
	if s.config.CanRead == nil {
		w.Header().Set("cache-control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("cache-control", "private, no-store")
	}
	http.ServeContent(w, r, sha, time.Unix(entry.Uploaded, 0), file)
}

func (s *Service) delete(w http.ResponseWriter, r *http.Request, sha string) {
	pubkey, err := s.authorize(r, ActionDelete)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, err)
		return
	}
	_, err = s.lookup(r.Context(), sha)
	if errors.Is(err, sql.ErrNoRows) {
		s.fail(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if !s.hasClaim(r.Context(), sha, pubkey) && !s.isOwner(pubkey) {
		s.fail(w, http.StatusForbidden, errors.New("restricted: not the uploader"))
		return
	}
	if s.isOwner(pubkey) {
		if err := s.Delete(r.Context(), sha); err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
	} else if err := s.DeleteForUploader(r.Context(), sha, pubkey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.fail(w, http.StatusNotFound, errors.New("not found"))
			return
		}
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	// Owners and uploaders may remove their claim; a shared physical object is
	// retained until its final claim disappears.
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) put(ctx context.Context, source io.Reader, typ, uploader, claimed string) (Blob, bool, error) {
	return s.putValidated(ctx, source, typ, uploader, claimed, nil)
}

func (s *Service) putValidated(ctx context.Context, source io.Reader, typ, uploader, claimed string, validate func(string) error) (Blob, bool, error) {
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
	return s.installUpload(ctx, uploadCandidate{path: tmpName, hash: sha, size: count, typ: typ, uploader: uploader})
}

type uploadCandidate struct {
	path, hash, typ, uploader, reservation string
	size                                   int64
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
	limits := s.currentLimits()
	if limits.MaxFileBytes > 0 && count > limits.MaxFileBytes {
		return Blob{}, false, ErrFileTooLarge
	}
	if limits.UserStorageBytes > 0 && uploader != "" && count > limits.UserStorageBytes {
		return Blob{}, false, ErrQuotaExceeded
	}
	if existing, err := s.lookup(ctx, sha); err == nil {
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
			if err := s.claim(ctx, sha, uploader); err != nil {
				return Blob{}, false, err
			}
		}
		if candidate.reservation != "" {
			if _, err := s.store.DB().ExecContext(ctx, "DELETE FROM multipart_uploads WHERE id=?", candidate.reservation); err != nil {
				return Blob{}, false, err
			}
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
	final := filepath.Join(s.root, sha)
	if err := os.Rename(candidate.path, final); err != nil && !errors.Is(err, os.ErrExist) {
		return Blob{}, false, fmt.Errorf("install blob: %w", err)
	}
	if err := syncDirectory(s.root); err != nil {
		return Blob{}, false, fmt.Errorf("sync blob directory: %w", err)
	}
	now := time.Now().UTC().Unix()
	typ = contentType(typ)
	if typ == "application/octet-stream" {
		typ = detectFileType(final)
	}
	entry := Blob{SHA256: sha, Size: count, Type: typ, Uploader: uploader, Uploaded: now}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM blob_tombstones WHERE sha256=?", entry.SHA256); err != nil {
			return fmt.Errorf("clear blob deletion tombstone: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO blobs(sha256,size,type,uploader,uploaded) VALUES(?,?,?,?,?)", entry.SHA256, entry.Size, entry.Type, entry.Uploader, entry.Uploaded); err != nil {
			return fmt.Errorf("record blob: %w", err)
		}
		if entry.Uploader != "" {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO blob_claims(sha256,uploader,claimed_at) VALUES(?,?,?)", entry.SHA256, entry.Uploader, entry.Uploaded); err != nil {
				return fmt.Errorf("record blob claim: %w", err)
			}
		}
		if candidate.reservation != "" {
			if _, err := tx.ExecContext(ctx, "DELETE FROM multipart_uploads WHERE id=?", candidate.reservation); err != nil {
				return fmt.Errorf("release upload reservation: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return Blob{}, false, err
	}
	if saved, err := s.lookup(ctx, sha); err == nil {
		return saved, saved.Uploader == uploader && saved.Uploaded == now, nil
	}
	return entry, true, nil
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

func (s *Service) claim(ctx context.Context, sha, uploader string) error {
	_, err := s.store.DB().ExecContext(ctx, "INSERT OR IGNORE INTO blob_claims(sha256,uploader,claimed_at) VALUES(?,?,?)", sha, uploader, time.Now().UTC().Unix())
	if err != nil {
		return fmt.Errorf("record blob claim: %w", err)
	}
	return nil
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

func (s *Service) list(w http.ResponseWriter, r *http.Request, pubkey string) {
	if !shaPattern.MatchString(pubkey) {
		s.fail(w, http.StatusBadRequest, errors.New("invalid: bad pubkey"))
		return
	}
	if _, err := s.authorize(r, ActionList); err != nil {
		s.fail(w, http.StatusUnauthorized, err)
		return
	}
	limit, err := parseListLimit(r.URL.Query().Get("limit"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	entries, _, err := s.ListPage(r.Context(), pubkey, limit, strings.TrimSpace(r.URL.Query().Get("cursor")))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.fail(w, http.StatusBadRequest, errors.New("invalid: cursor does not identify a blob"))
			return
		}
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	descriptors := make([]map[string]any, 0, len(entries))
	for _, b := range entries {
		descriptors = append(descriptors, s.descriptor(r, b))
	}
	s.writeJSON(w, http.StatusOK, descriptors)
}

func (s *Service) report(w http.ResponseWriter, r *http.Request) {
	reporter, err := s.authorize(r, ActionReport)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, err)
		return
	}
	var input event.Event
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Kind != 1984 || input.PubKey != reporter {
		s.fail(w, http.StatusBadRequest, errors.New("invalid: report must be a signed kind 1984 event"))
		return
	}
	if err := event.Validate(input); err != nil || time.Since(time.Unix(input.CreatedAt, 0)) > time.Hour || time.Until(time.Unix(input.CreatedAt, 0)) > time.Hour {
		s.fail(w, http.StatusBadRequest, errors.New("invalid: report is not a current signed kind 1984 event"))
		return
	}
	filed := 0
	type reportTarget struct {
		hash     string
		uploader string
	}
	targets := make([]reportTarget, 0, len(input.Tags))
	for _, tag := range input.Tags {
		if len(tag) < 2 || tag[0] != "x" || !shaPattern.MatchString(tag[1]) {
			continue
		}
		entry, lookupErr := s.lookup(r.Context(), tag[1])
		if lookupErr != nil {
			continue
		}
		id := input.ID
		if filed > 0 {
			id += "." + strconv.Itoa(filed)
		}
		targets = append(targets, reportTarget{hash: tag[1], uploader: entry.Uploader})
	}
	if len(targets) == 0 {
		s.fail(w, http.StatusNotFound, errors.New("not found: relay holds none of the reported blobs"))
		return
	}
	err = s.store.WithTx(r.Context(), func(tx *sql.Tx) error {
		for i, target := range targets {
			id := input.ID
			if i > 0 {
				id += "." + strconv.Itoa(i)
			}
			result, insertErr := tx.ExecContext(r.Context(), `INSERT OR IGNORE INTO blob_reports(id,reporter,target_pubkey,target_blob,type,content,at) VALUES(?,?,?,?,?,?,?)`, id, reporter, target.uploader, target.hash, "blob", input.Content, input.CreatedAt)
			if insertErr != nil {
				return insertErr
			}
			changed, rowsErr := result.RowsAffected()
			if rowsErr != nil {
				return rowsErr
			}
			if changed == 0 {
				continue
			}
			if s.config.RecordReport != nil {
				if err := s.config.RecordReport(r.Context(), tx, reporter, target.hash, "blob", "blob", input.Content); err != nil {
					return err
				}
			}
			filed++
		}
		return nil
	})
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if filed == 0 {
		// Repeating an identical report is idempotent and still successful.
		filed = len(targets)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "filed": filed})
}

// Block permanently prevents a hash from returning through any upload door.
func (s *Service) Block(ctx context.Context, sha, reason string) error {
	if !shaPattern.MatchString(sha) {
		return errors.New("invalid: bad blob sha256")
	}
	_, err := s.store.DB().ExecContext(ctx, "INSERT INTO blob_blocks(sha256,reason,blocked_at) VALUES(?,?,?) ON CONFLICT(sha256) DO UPDATE SET reason=excluded.reason,blocked_at=excluded.blocked_at", sha, reason, time.Now().UTC().Unix())
	if err != nil {
		return fmt.Errorf("block blob: %w", err)
	}
	if _, err := s.store.DB().ExecContext(ctx, "INSERT INTO blob_tombstones(sha256,deleted_at) VALUES(?,?) ON CONFLICT(sha256) DO UPDATE SET deleted_at=excluded.deleted_at", sha, time.Now().UTC().Unix()); err != nil {
		return fmt.Errorf("record blocked blob deletion: %w", err)
	}
	if _, err := s.store.DB().ExecContext(ctx, "DELETE FROM blobs WHERE sha256=?", sha); err != nil {
		return fmt.Errorf("remove blocked blob metadata: %w", err)
	}
	if err := os.Remove(filepath.Join(s.root, sha)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove blocked blob: %w", err)
	}
	return nil
}

func (s *Service) mirror(w http.ResponseWriter, r *http.Request) {
	pubkey, err := s.authorize(r, ActionMirror)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, err)
		return
	}
	var input struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.URL == "" {
		s.fail(w, http.StatusBadRequest, errors.New("invalid: body needs a url"))
		return
	}
	requestedURL := input.URL
	expectedHash := ""
	extension := ""
	uriServers := []string{}
	authors := []string{}
	if strings.HasPrefix(strings.ToLower(requestedURL), "blossom:") {
		uri, err := ParseBlossomURI(requestedURL)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		expectedHash, extension = uri.Hash, uri.Extension
		uriServers = append(uriServers, uri.Servers...)
		authors = append(authors, uri.Authors...)
		requestedURL = ""
	} else if hash := BlobHashFromURL(requestedURL); hash != "" {
		expectedHash = hash
	}
	client := s.config.HTTPClient
	if client == nil {
		client = s.safeClient()
	}
	candidates := []string{}
	if requestedURL != "" {
		candidates = append(candidates, requestedURL)
	}
	candidates = append(candidates, FallbackURLs(uriServers, expectedHash, extension)...)
	if s.config.ResolveServers != nil && expectedHash != "" {
		if len(authors) == 0 && pubkey != "" {
			authors = []string{pubkey}
		}
		for _, author := range uniqueStrings(authors) {
			servers, resolveErr := s.config.ResolveServers(r.Context(), author)
			if resolveErr != nil {
				continue
			}
			candidates = append(candidates, FallbackURLs(servers, expectedHash, extension)...)
		}
	}
	var lastErr error
	for _, candidate := range uniqueStrings(candidates) {
		if err := validateMirrorURL(candidate, s.resolve); err != nil {
			lastErr = err
			continue
		}
		response, fetchErr := client.Get(candidate)
		if fetchErr != nil {
			lastErr = fetchErr
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			lastErr = fmt.Errorf("error: origin answered %d", response.StatusCode)
			_ = response.Body.Close()
			continue
		}
		typ := contentType(response.Header.Get("content-type"))
		if typ == "application/octet-stream" {
			typ = typeForExtension(candidate)
		}
		entry, created, putErr := s.putValidated(r.Context(), response.Body, typ, pubkey, expectedHash, func(actual string) error {
			if s.config.ValidateUpload != nil {
				return s.config.ValidateUpload(r, actual)
			}
			return nil
		})
		_ = response.Body.Close()
		if putErr == nil {
			s.writeJSON(w, chooseStatus(created, http.StatusCreated), s.descriptor(r, entry))
			return
		}
		lastErr = putErr
		if !errors.Is(putErr, ErrHashMismatch) {
			s.fail(w, statusFor(putErr), putErr)
			return
		}
	}
	if lastErr == nil {
		lastErr = errors.New("error: no mirror origin available")
	}
	s.fail(w, statusFor(lastErr), lastErr)
}

func (s *Service) nip96(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		pubkey, err := s.authorize(r, ActionList)
		if err != nil {
			s.fail(w, http.StatusUnauthorized, err)
			return
		}
		s.nip96List(w, r, pubkey)
		return
	}
	if r.Method != http.MethodPost {
		s.fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	pubkey, err := s.authorize(r, ActionUpload)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, err)
		return
	}
	if err := r.ParseMultipartForm(0); err != nil {
		s.fail(w, http.StatusBadRequest, errors.New("invalid: body is not multipart form data"))
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		s.fail(w, http.StatusBadRequest, errors.New("invalid: form needs a file field"))
		return
	}
	defer file.Close()
	typ := contentType(header.Header.Get("Content-Type"))
	if typ == "application/octet-stream" {
		typ = contentType(r.FormValue("content_type"))
	}
	entry, created, err := s.put(r.Context(), file, typ, pubkey, "")
	if err != nil {
		s.fail(w, statusFor(err), err)
		return
	}
	s.writeJSON(w, chooseStatus(created, http.StatusCreated), map[string]any{"status": "success", "message": "Upload successful.", "url": s.blobURL(r, entry), "nip94_event": map[string]any{"tags": s.nip94Tags(r, entry), "content": r.FormValue("caption")}})
}

func (s *Service) nip96List(w http.ResponseWriter, r *http.Request, pubkey string) {
	count := parsePageCount(r.URL.Query().Get("count"))
	page := parsePage(r.URL.Query().Get("page"))
	var total int
	if err := s.store.DB().QueryRowContext(r.Context(), "SELECT count(*) FROM blob_claims WHERE uploader=?", pubkey).Scan(&total); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	rows, err := s.store.DB().QueryContext(r.Context(), `SELECT b.sha256,b.size,b.type,c.uploader,b.uploaded
		FROM blobs b JOIN blob_claims c ON c.sha256=b.sha256 WHERE c.uploader=?
		ORDER BY b.uploaded DESC,b.sha256 DESC LIMIT ? OFFSET ?`, pubkey, count, page*count)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	files := make([]map[string]any, 0)
	for rows.Next() {
		var b Blob
		if err := rows.Scan(&b.SHA256, &b.Size, &b.Type, &b.Uploader, &b.Uploaded); err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		files = append(files, map[string]any{"tags": s.nip94Tags(r, b), "content": "", "created_at": b.Uploaded})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"count": count, "total": total, "page": page, "files": files})
}

func (s *Service) authorize(r *http.Request, action Action) (string, error) {
	if s.config.Authorize == nil {
		return "", errors.New("auth-required: authorization is required")
	}
	return s.config.Authorize(r, action)
}
func (s *Service) isOwner(pubkey string) bool {
	return s.config.IsOwner != nil && s.config.IsOwner(pubkey)
}
func (s *Service) descriptor(r *http.Request, b Blob) map[string]any {
	return map[string]any{"url": s.blobURL(r, b), "sha256": b.SHA256, "size": b.Size, "type": b.Type, "uploaded": b.Uploaded, "nip94": s.nip94Tags(r, b)}
}
func (s *Service) nip94Tags(r *http.Request, b Blob) [][]string {
	return [][]string{{"url", s.blobURL(r, b)}, {"m", b.Type}, {"x", b.SHA256}, {"ox", b.SHA256}, {"size", strconv.FormatInt(b.Size, 10)}}
}
func (s *Service) blobURL(r *http.Request, b Blob) string {
	return s.origin(r) + "/" + b.SHA256 + extensionSuffix(b.Type)
}
func (s *Service) nip96Info(r *http.Request) map[string]any {
	return map[string]any{"api_url": s.origin(r) + "/nip96", "download_url": s.origin(r), "supported_nips": []int{94, 98}, "content_types": contentTypes(), "plans": map[string]any{"self-hosted": map[string]any{"name": "Self-hosted", "is_nip98_required": true, "max_byte_size": 0, "file_expiration": []int{0, 0}, "media_transformations": map[string]any{}}}}
}
func (s *Service) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Service) fail(w http.ResponseWriter, status int, err error) {
	w.Header().Set("x-reason", err.Error())
	s.writeJSON(w, status, map[string]string{"error": err.Error()})
}
func origin(r *http.Request) string {
	scheme := r.URL.Scheme
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	return scheme + "://" + r.Host
}

func (s *Service) origin(r *http.Request) string {
	if s.config.PublicURL != "" {
		return strings.TrimRight(s.config.PublicURL, "/")
	}
	return origin(r)
}

// DetectMediaType recognises images, video, audio and PDF from the first
// bytes of a blob. Anything else stays an octet stream so uploads can never
// be reinterpreted as a page or a script.
func DetectMediaType(head []byte) string {
	detected := contentType(http.DetectContentType(head))
	for _, prefix := range []string{"image/", "video/", "audio/"} {
		if strings.HasPrefix(detected, prefix) {
			return detected
		}
	}
	if detected == "application/pdf" {
		return detected
	}
	return "application/octet-stream"
}

func detectFileType(name string) string {
	file, err := os.Open(name)
	if err != nil {
		return "application/octet-stream"
	}
	defer file.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	return DetectMediaType(head[:n])
}

func contentType(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(strings.Split(raw, ";")[0]))
	if raw == "" {
		return "application/octet-stream"
	}
	return raw
}
func extensionSuffix(typ string) string {
	if ext := extensions[typ]; ext != "" {
		return "." + ext
	}
	return ""
}

func typeForExtension(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "application/octet-stream"
	}
	name := strings.ToLower(filepath.Ext(u.Path))
	for typ, ext := range extensions {
		if "."+ext == name {
			return typ
		}
	}
	return "application/octet-stream"
}
func contentTypes() []string {
	out := make([]string, 0, len(extensions))
	for typ := range extensions {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}

func parsePageCount(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 20
	}
	if value > 100 {
		return 100
	}
	return value
}

func parsePage(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func parseListLimit(raw string) (int, error) {
	if raw == "" {
		return 20, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 100 {
		return 0, errors.New("invalid: limit must be between 1 and 100")
	}
	return value, nil
}
func chooseStatus(created bool, status int) int {
	if created {
		return status
	}
	return http.StatusOK
}
func statusFor(err error) int {
	if errors.Is(err, ErrHashMismatch) {
		return http.StatusConflict
	}
	if errors.Is(err, ErrMultipartConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, ErrFileTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	if errors.Is(err, ErrQuotaExceeded) {
		return http.StatusForbidden
	}
	if errors.Is(err, ErrBlocked) {
		return http.StatusForbidden
	}
	if strings.HasPrefix(err.Error(), "auth-required:") {
		return http.StatusUnauthorized
	}
	if strings.HasPrefix(err.Error(), "invalid:") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok || value == "" {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func validateMirrorURL(raw string, resolve ResolveIP) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("invalid: only https urls can be mirrored")
	}
	ips, err := resolve(context.Background(), u.Hostname())
	if err != nil || len(ips) == 0 {
		return errors.New("invalid: origin hostname did not resolve")
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return errors.New("invalid: origin resolves to a private address")
		}
	}
	return nil
}

func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return false
		}
		if ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0 {
			return false
		}
		if ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19) {
			return false
		}
	}
	return true
}
func (s *Service) safeClient() *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	transport := base.Clone()
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := s.resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, errors.New("origin has no reachable public address")
	}
	return &http.Client{Timeout: 5 * time.Minute, Transport: transport, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		return validateMirrorURL(req.URL.String(), s.resolve)
	}}
}
