package blob

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Action identifies the file-door operation being authorized.
type Action string

const (
	ActionUpload Action = "upload"
	ActionGet    Action = "get"
	ActionDelete Action = "delete"
	ActionList   Action = "list"
	ActionMirror Action = "mirror"
	ActionReport Action = "report"
)

// Authorize authenticates an HTTP request for an action and returns the
// caller's pubkey.
type Authorize func(request *http.Request, action Action) (pubkey string, err error)

// CanRead reports whether any supplied reader pubkey may read the blob.
type CanRead func(ctx context.Context, blobSHA string, readerPubkeys []string) bool

// ResolveIP resolves a host for mirror URL policy checks.
type ResolveIP func(ctx context.Context, host string) ([]net.IP, error)

// ReportRecorder records a report in the host moderation transaction. The
// targetKind and reportType values describe the reported object and report;
// blob reports currently pass "blob" for both.
type ReportRecorder func(ctx context.Context, tx *sql.Tx, reporter string, targetBlob string, targetKind string, reportType string, content string) error

// Config supplies durable storage, HTTP policy and host integration hooks.
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
	// UploadTerms returns what an uploader's standing imposes on its uploads:
	// an agent grant with a sites ttl gives its claims an expiry, and one with
	// the encrypted flag refuses plain uploads. Zero values mean no terms.
	UploadTerms func(context.Context, string) (UploadTerms, error)
}

// Limits are applied when each upload starts. Zero leaves that limit open.
type Limits struct {
	MaxFileBytes     int64
	UserStorageBytes int64
}

// UploadTerms is what an uploader's grant imposes on the blobs it stores.
// ExpiresAt is the unix time its claims lapse, 0 for never; Encrypted
// requires the stored bytes to be an encrypted blob or manifest.
type UploadTerms struct {
	ExpiresAt int64
	Encrypted bool
}

// encryptedTypes are the content types the browser uploader gives encrypted
// objects: AES-GCM ciphertext for files and chunks, and the BUD-16 directory
// type for encrypted manifests. Anything the relay recognizes as a page,
// image, video, audio or document is plain.
var encryptedTypes = map[string]bool{"application/octet-stream": true, "application/vnd.blossom.directory+msgpack": true}

// Blob is the metadata for one content-addressed object.
type Blob struct {
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Type     string `json:"type"`
	Uploader string `json:"uploader"`
	Uploaded int64  `json:"uploaded"`
}

// BlobClaimMetadata names a blob for one uploader. Names are claim-scoped so
// deduplicated blobs can have different names for different uploaders.
type BlobClaimMetadata struct {
	SHA256    string `json:"sha256"`
	Uploader  string `json:"uploader"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	UpdatedAt int64  `json:"updated_at"`
}

// PutOptions describes a blob upload and its optional metadata transaction.
type PutOptions struct {
	Reader   io.Reader
	Type     string
	Uploader string
	Hash     string
	Name     string
	Path     string
	// Commit records ownership metadata in the blob transaction. Its isNew
	// argument reports whether the blob was newly installed. An error rolls
	// back all claims.
	// New bytes remain unavailable until their ownership metadata is durable.
	Commit func(ctx context.Context, tx *sql.Tx, blob Blob, isNew bool) error
}

// Service owns blob files and their metadata operations.
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
var ErrNotEncrypted = errors.New("restricted: agent grant requires encrypted uploads")

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
	CREATE TABLE IF NOT EXISTS blob_claim_metadata (
	 sha256 TEXT NOT NULL, uploader TEXT NOT NULL, name TEXT NOT NULL, path TEXT NOT NULL,
	 updated_at INTEGER NOT NULL, PRIMARY KEY (sha256,uploader,path),
	 FOREIGN KEY (sha256,uploader) REFERENCES blob_claims(sha256,uploader) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS blob_claim_metadata_uploader ON blob_claim_metadata(uploader,updated_at DESC,sha256 DESC);
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
	CREATE TABLE IF NOT EXISTS multipart_uploads (id TEXT PRIMARY KEY, sha256 TEXT NOT NULL, uploader TEXT NOT NULL, length INTEGER NOT NULL, type TEXT NOT NULL, created INTEGER NOT NULL, last_seen INTEGER NOT NULL, name TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '');
	CREATE TABLE IF NOT EXISTS multipart_parts (upload_id TEXT NOT NULL, offset INTEGER NOT NULL, length INTEGER NOT NULL, PRIMARY KEY(upload_id, offset), FOREIGN KEY(upload_id) REFERENCES multipart_uploads(id) ON DELETE CASCADE);
	INSERT OR IGNORE INTO blob_claims(sha256,uploader,claimed_at)
		SELECT sha256,uploader,uploaded FROM blobs WHERE uploader != ''`); err != nil {
		return nil, fmt.Errorf("initialize blob metadata: %w", err)
	}
	// Claims made under an agent grant with a ttl carry an expiry; every
	// earlier claim keeps 0, which never lapses.
	if _, err := config.Store.DB().ExecContext(ctx, "ALTER TABLE blob_claims ADD COLUMN expires INTEGER NOT NULL DEFAULT 0"); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("add blob claim expiry: %w", err)
	}
	for _, column := range []string{"name TEXT NOT NULL DEFAULT ''", "path TEXT NOT NULL DEFAULT ''"} {
		if _, err := config.Store.DB().ExecContext(ctx, "ALTER TABLE multipart_uploads ADD COLUMN "+column); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("add multipart metadata: %w", err)
		}
	}
	if _, err := config.Store.DB().ExecContext(ctx, "CREATE INDEX IF NOT EXISTS blob_claims_expires ON blob_claims(expires) WHERE expires>0"); err != nil {
		return nil, fmt.Errorf("index blob claim expiry: %w", err)
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
