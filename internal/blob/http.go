package blob

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

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
