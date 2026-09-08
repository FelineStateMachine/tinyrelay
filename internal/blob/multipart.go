package blob

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	multipartExpiry           = 60 * time.Second
	multipartChunkLimit       = 8 << 20
	multipartStorageLimit     = 1 << 30
	multipartSessionLimit     = 64
	multipartUserSessionLimit = 16
	multipartActiveLimit      = 32
	multipartRangeLimit       = 1024
)

type multipartRequest struct {
	id, hash, uploader, typ string
	length, offset, count   int64
}

type multipartError struct {
	status  int
	message string
}

func (e *multipartError) Error() string                 { return e.message }
func multipartFailure(status int, message string) error { return &multipartError{status, message} }
func (s *Service) failMultipart(w http.ResponseWriter, err error) {
	var failure *multipartError
	if errors.As(err, &failure) {
		s.fail(w, failure.status, err)
		return
	}
	s.fail(w, statusFor(err), err)
}

func (s *Service) multipart(w http.ResponseWriter, r *http.Request, hash string) {
	uploader, err := s.authorize(r, ActionUpload)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, err)
		return
	}
	part, err := parseMultipart(r, hash, uploader)
	if err != nil {
		s.failMultipart(w, err)
		return
	}
	if err := s.reserveMultipart(r.Context(), part); err != nil {
		s.failMultipart(w, err)
		return
	}
	defer s.releaseMultipart(part.id)
	chunk, err := s.stageMultipart(r, part)
	if err != nil {
		s.failMultipart(w, err)
		return
	}
	defer os.Remove(chunk)
	entry, created, complete, err := s.commitMultipart(r.Context(), part, chunk)
	if err != nil {
		s.failMultipart(w, err)
		return
	}
	if !complete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeJSON(w, chooseStatus(created, http.StatusCreated), s.descriptor(r, entry))
}

func parseMultipart(r *http.Request, hash, uploader string) (multipartRequest, error) {
	part := multipartRequest{id: multipartID(hash, uploader), hash: hash, uploader: uploader, count: r.ContentLength}
	if r.ContentLength < 0 {
		return part, multipartFailure(411, "Content-Length is required")
	}
	if r.Header.Get("Content-Type") != "application/octet-stream" {
		return part, multipartFailure(415, "Content-Type must be application/octet-stream")
	}
	for _, item := range []struct {
		name   string
		target *int64
	}{{"Upload-Length", &part.length}, {"Upload-Offset", &part.offset}} {
		raw := r.Header.Get(item.name)
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 || raw == "" || strings.Trim(raw, "0123456789") != "" {
			return part, multipartFailure(400, item.name+" must be a nonnegative integer")
		}
		*item.target = value
	}
	if part.offset > part.length || part.count > part.length-part.offset {
		return part, multipartFailure(416, "chunk range exceeds Upload-Length")
	}
	if part.count == 0 && part.length != 0 {
		return part, multipartFailure(400, "nonempty uploads require nonempty chunks")
	}
	if part.count > multipartChunkLimit || part.length > multipartStorageLimit {
		return part, multipartFailure(413, "multipart upload exceeds temporary storage limits")
	}
	typ, _, err := mime.ParseMediaType(r.Header.Get("Upload-Type"))
	if err != nil || !strings.Contains(typ, "/") {
		return part, multipartFailure(400, "Upload-Type must be a MIME type")
	}
	part.typ = contentType(typ)
	return part, nil
}

func multipartID(hash, uploader string) string {
	digest := sha256.Sum256([]byte(uploader))
	return hash + "-" + hex.EncodeToString(digest[:])
}
func (s *Service) multipartPath(id string) string { return filepath.Join(s.root, ".multipart-"+id) }

// Admission reserves the final length before reading any request bytes. Each
// active chunk is also bounded, so unlimited durable storage does not mean
// unlimited temporary files or sessions.
func (s *Service) reserveMultipart(ctx context.Context, part multipartRequest) error {
	s.multipartMu.Lock()
	defer s.multipartMu.Unlock()
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	if err := s.cleanupMultipartLocked(ctx); err != nil {
		return err
	}
	active := 0
	for _, count := range s.activeParts {
		active += count
	}
	if active >= multipartActiveLimit {
		return multipartFailure(429, "too many active upload chunks")
	}
	if err := s.multipartAdmission(ctx, part); err != nil {
		return err
	}
	s.activeParts[part.id]++
	return nil
}

func (s *Service) multipartAdmission(ctx context.Context, part multipartRequest) error {
	if s.blocked(ctx, part.hash) {
		return ErrBlocked
	}
	limits := s.currentLimits()
	if limits.MaxFileBytes > 0 && part.length > limits.MaxFileBytes {
		return ErrFileTooLarge
	}
	existing, lookupErr := s.lookup(ctx, part.hash)
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return lookupErr
	}
	if lookupErr == nil {
		if existing.Size != part.length {
			return ErrMultipartConflict
		}
		return s.checkMultipartQuota(ctx, part, limits)
	}
	var length int64
	var typ string
	err := s.store.DB().QueryRowContext(ctx, "SELECT length,type FROM multipart_uploads WHERE id=?", part.id).Scan(&length, &typ)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && (length != part.length || typ != part.typ) {
		return ErrMultipartConflict
	}
	if err := s.checkMultipartQuota(ctx, part, limits); err != nil {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return s.createMultipart(ctx, part)
	}
	_, err = s.store.DB().ExecContext(ctx, "UPDATE multipart_uploads SET last_seen=? WHERE id=?", time.Now().Unix(), part.id)
	return err
}

func (s *Service) checkMultipartQuota(ctx context.Context, part multipartRequest, limits Limits) error {
	if limits.UserStorageBytes == 0 || s.hasClaim(ctx, part.hash, part.uploader) {
		return nil
	}
	used, err := s.quotaUsageExcept(ctx, part.uploader, part.id)
	if err != nil {
		return err
	}
	if part.length > limits.UserStorageBytes-used {
		return ErrQuotaExceeded
	}
	return nil
}

func (s *Service) createMultipart(ctx context.Context, part multipartRequest) error {
	var count, userCount int
	var total int64
	err := s.store.DB().QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length),0),COALESCE(SUM(uploader=?),0) FROM multipart_uploads`, part.uploader).Scan(&count, &total, &userCount)
	if err != nil {
		return err
	}
	if count >= multipartSessionLimit || userCount >= multipartUserSessionLimit || part.length > multipartStorageLimit-total {
		return multipartFailure(429, "temporary upload capacity is full")
	}
	path := s.multipartPath(part.id)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	err = errors.Join(file.Truncate(part.length), file.Sync(), file.Close())
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	now := time.Now().Unix()
	_, err = s.store.DB().ExecContext(ctx, "INSERT INTO multipart_uploads(id,sha256,uploader,length,type,created,last_seen) VALUES(?,?,?,?,?,?,?)", part.id, part.hash, part.uploader, part.length, part.typ, now, now)
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

func (s *Service) stageMultipart(r *http.Request, part multipartRequest) (string, error) {
	chunk, err := os.CreateTemp(s.root, ".multipart-chunk-")
	if err != nil {
		return "", err
	}
	path := chunk.Name()
	keep := false
	defer func() {
		_ = chunk.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	hasher := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(chunk, hasher), r.Body, part.count); err != nil {
		return "", multipartFailure(400, "chunk body is shorter than Content-Length")
	}
	var extra [1]byte
	if n, err := io.ReadFull(r.Body, extra[:]); n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return "", multipartFailure(400, "chunk body is longer than Content-Length")
	}
	if err := r.Context().Err(); err != nil {
		return "", err
	}
	if s.config.ValidateUpload != nil {
		if err := s.config.ValidateUpload(r, hex.EncodeToString(hasher.Sum(nil))); err != nil {
			return "", err
		}
	}
	if err := chunk.Close(); err != nil {
		return "", err
	}
	keep = true
	return path, nil
}

func (s *Service) releaseMultipart(id string) {
	s.multipartMu.Lock()
	defer s.multipartMu.Unlock()
	s.activeParts[id]--
	if s.activeParts[id] > 0 {
		return
	}
	delete(s.activeParts, id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var parts int
	if err := s.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM multipart_parts WHERE upload_id=?", id).Scan(&parts); err == nil && parts == 0 {
		s.quotaMu.Lock()
		defer s.quotaMu.Unlock()
		_ = s.discardMultipart(ctx, id)
	}
}

func (s *Service) commitMultipart(ctx context.Context, part multipartRequest, chunk string) (Blob, bool, bool, error) {
	s.multipartMu.Lock()
	defer s.multipartMu.Unlock()
	// Recheck policy after streaming, including uploads whose next chunk does
	// not complete the file. Policy may have changed while the body arrived.
	s.quotaMu.Lock()
	admissionErr := s.multipartAdmission(ctx, part)
	s.quotaMu.Unlock()
	if admissionErr != nil {
		return Blob{}, false, false, admissionErr
	}
	candidate := uploadCandidate{hash: part.hash, size: part.length, typ: part.typ, uploader: part.uploader, reservation: part.id}
	if existing, err := s.lookup(ctx, part.hash); err == nil {
		if existing.Size != part.length {
			return Blob{}, false, false, ErrMultipartConflict
		}
		entry, _, err := s.installUpload(ctx, candidate)
		return entry, false, err == nil, err
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Blob{}, false, false, err
	}
	ranges, err := s.multipartRanges(ctx, part)
	if err != nil {
		return Blob{}, false, false, err
	}
	candidate.path = s.multipartPath(part.id)
	if err := writeMultipartChunk(candidate.path, chunk, part.offset); err != nil {
		_ = s.discardMultipart(ctx, part.id)
		return Blob{}, false, false, err
	}
	if err := s.recordMultipartRanges(ctx, part.id, ranges); err != nil {
		_ = s.discardMultipart(ctx, part.id)
		return Blob{}, false, false, err
	}
	if len(ranges) != 1 || ranges[0].start != 0 || ranges[0].end != part.length {
		return Blob{}, false, false, nil
	}
	if err := verifyMultipart(candidate.path, part.hash, part.length); err != nil {
		return Blob{}, false, false, errors.Join(err, s.discardMultipart(ctx, part.id))
	}
	entry, created, err := s.installUpload(ctx, candidate)
	cleanupErr := s.discardMultipart(ctx, part.id)
	return entry, created, err == nil && cleanupErr == nil, errors.Join(err, cleanupErr)
}

type multipartRange struct{ start, end int64 }

func (s *Service) multipartRanges(ctx context.Context, part multipartRequest) ([]multipartRange, error) {
	rows, err := s.store.DB().QueryContext(ctx, "SELECT offset,length FROM multipart_parts WHERE upload_id=? ORDER BY offset", part.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ranges := []multipartRange{{part.offset, part.offset + part.count}}
	for rows.Next() {
		var offset, length int64
		if err := rows.Scan(&offset, &length); err != nil {
			return nil, err
		}
		ranges = append(ranges, multipartRange{offset, offset + length})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	merged := []multipartRange{ranges[0]}
	for _, span := range ranges[1:] {
		last := &merged[len(merged)-1]
		if span.start <= last.end {
			last.end = max(last.end, span.end)
		} else {
			merged = append(merged, span)
		}
	}
	if len(merged) > multipartRangeLimit {
		return nil, multipartFailure(429, "too many disjoint upload ranges")
	}
	return merged, nil
}

func writeMultipartChunk(path, chunk string, offset int64) error {
	source, err := os.Open(chunk)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer target.Close()
	if _, err := target.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		return err
	}
	return target.Sync()
}
func (s *Service) recordMultipartRanges(ctx context.Context, id string, ranges []multipartRange) error {
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM multipart_parts WHERE upload_id=?", id); err != nil {
			return err
		}
		for _, span := range ranges {
			if _, err := tx.ExecContext(ctx, "INSERT INTO multipart_parts(upload_id,offset,length) VALUES(?,?,?)", id, span.start, span.end-span.start); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE multipart_uploads SET last_seen=? WHERE id=?", time.Now().Unix(), id)
		return err
	})
}
func verifyMultipart(path, hash string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	count, err := io.Copy(hasher, file)
	if err != nil {
		return err
	}
	if count != size || hex.EncodeToString(hasher.Sum(nil)) != hash {
		return ErrHashMismatch
	}
	return nil
}
func (s *Service) discardMultipart(ctx context.Context, id string) error {
	_, err := s.store.DB().ExecContext(ctx, "DELETE FROM multipart_uploads WHERE id=?", id)
	if err != nil {
		return err
	}
	if err := os.Remove(s.multipartPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// CleanupMultipart releases inactive reservations and removes partial bytes.
// The tenant maintenance scheduler calls it even when no uploads arrive.
func (s *Service) CleanupMultipart(ctx context.Context) error {
	s.multipartMu.Lock()
	defer s.multipartMu.Unlock()
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	return s.cleanupMultipartLocked(ctx)
}
func (s *Service) cleanupMultipartLocked(ctx context.Context) error {
	rows, err := s.store.DB().QueryContext(ctx, "SELECT id FROM multipart_uploads WHERE last_seen <= ?", time.Now().Add(-multipartExpiry).Unix())
	if err != nil {
		return err
	}
	var expired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		if s.activeParts[id] == 0 {
			expired = append(expired, id)
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, id := range expired {
		if err := s.discardMultipart(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) reconcileMultipart(ctx context.Context) error {
	if err := s.CleanupMultipart(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".multipart-") || entry.IsDir() {
			continue
		}
		id := strings.TrimPrefix(entry.Name(), ".multipart-")
		var length int64
		err := s.store.DB().QueryRowContext(ctx, "SELECT length FROM multipart_uploads WHERE id=?", id).Scan(&length)
		if errors.Is(err, sql.ErrNoRows) {
			if err := os.Remove(filepath.Join(s.root, entry.Name())); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() != length {
			if err := s.discardMultipart(ctx, id); err != nil {
				return err
			}
		}
	}
	// A reservation with a missing file cannot represent acknowledged bytes.
	rows, err := s.store.DB().QueryContext(ctx, "SELECT id FROM multipart_uploads")
	if err != nil {
		return err
	}
	var missing []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		if _, err := os.Stat(s.multipartPath(id)); errors.Is(err, os.ErrNotExist) {
			missing = append(missing, id)
		} else if err != nil {
			_ = rows.Close()
			return err
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, id := range missing {
		if err := s.discardMultipart(ctx, id); err != nil {
			return fmt.Errorf("reconcile multipart upload: %w", err)
		}
	}
	return nil
}
