package blob

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxClaimMetadataBytes = 4096

var (
	errInvalidClaimName    = errors.New("invalid: filename must be a single path component")
	errInvalidClaimPath    = errors.New("invalid: upload path must be a safe relative path")
	errMismatchedClaimName = errors.New("invalid: filename must match the upload path basename")
)

func validateClaimName(name string) error {
	if name == "" {
		return nil
	}
	if !utf8.ValidString(name) || len(name) > maxClaimMetadataBytes || strings.ContainsAny(name, "/\\") || name == "." || name == ".." || hasControl(name) {
		return errInvalidClaimName
	}
	return nil
}

func validateClaimPath(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if !utf8.ValidString(raw) || len(raw) > maxClaimMetadataBytes || strings.Contains(raw, "\\") || strings.HasPrefix(raw, "/") || strings.HasSuffix(raw, "/") || hasControl(raw) {
		return "", errInvalidClaimPath
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != raw {
		return "", errInvalidClaimPath
	}
	for _, part := range strings.Split(raw, "/") {
		if part == "" || part == "." || part == ".." {
			return "", errInvalidClaimPath
		}
	}
	return clean, nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func claimMetadata(name, rawPath string) (string, string, error) {
	if err := validateClaimName(name); err != nil {
		return "", "", err
	}
	cleanPath, err := validateClaimPath(rawPath)
	if err != nil {
		return "", "", err
	}
	if name == "" && cleanPath != "" {
		name = path.Base(cleanPath)
	}
	if cleanPath == "" {
		cleanPath = name
	} else if name != "" && path.Base(cleanPath) != name {
		return "", "", errMismatchedClaimName
	}
	return name, cleanPath, nil
}

func (s *Service) saveClaimMetadata(ctx context.Context, tx *sql.Tx, sha, uploader, name, metadataPath string) error {
	if uploader == "" || (name == "" && metadataPath == "") {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO blob_claim_metadata(sha256,uploader,name,path,updated_at)
		VALUES(?,?,?,?,?) ON CONFLICT(sha256,uploader,path) DO UPDATE SET name=excluded.name,updated_at=excluded.updated_at`,
		sha, uploader, name, metadataPath, nowUnix())
	if err != nil {
		return fmt.Errorf("record blob claim metadata: %w", err)
	}
	return nil
}

func (s *Service) ListClaimMetadata(ctx context.Context, uploader string) ([]BlobClaimMetadata, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT c.sha256,c.uploader,COALESCE(m.name,''),COALESCE(m.path,''),COALESCE(m.updated_at,c.claimed_at)
		FROM blob_claims c LEFT JOIN blob_claim_metadata m ON m.sha256=c.sha256 AND m.uploader=c.uploader
		JOIN blobs b ON b.sha256=c.sha256 WHERE c.uploader=? ORDER BY COALESCE(m.updated_at,c.claimed_at) DESC,c.sha256 DESC`, uploader)
	if err != nil {
		return nil, fmt.Errorf("list blob claim metadata: %w", err)
	}
	defer rows.Close()
	entries := make([]BlobClaimMetadata, 0)
	for rows.Next() {
		var entry BlobClaimMetadata
		if err := rows.Scan(&entry.SHA256, &entry.Uploader, &entry.Name, &entry.Path, &entry.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan blob claim metadata: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func nowUnix() int64 { return time.Now().UTC().Unix() }
