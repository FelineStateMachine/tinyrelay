package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/auth"
	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/community"
)

const roomAttachmentMaxBytes int64 = 32 << 20

var roomAttachmentPath = regexp.MustCompile(`^/rooms/([a-z0-9_-]{1,64})/attachments$`)

var mediaPath = regexp.MustCompile(`^/media/([0-9a-f]{64})(?:\.([a-z0-9]{1,8}))?$`)

type roomAttachmentScope struct {
	RoomID, EventID string
}

type roomAttachmentInput struct {
	Body     []byte
	Type     string
	Filename string
}

// storeRoomAttachment applies the same upload policy to browser and MCP calls.
func (t *Tenant) storeRoomAttachment(ctx context.Context, actor, roomID string, input roomAttachmentInput) (map[string]any, error) {
	if err := t.roomAttachmentUploader(ctx, actor, roomID); err != nil {
		return nil, err
	}
	room, _, err := t.roomFor(ctx, actor, roomID)
	if err != nil {
		return nil, err
	}
	if len(input.Body) == 0 || int64(len(input.Body)) > roomAttachmentMaxBytes {
		return nil, errors.New("invalid: attachment must be between 1 byte and 32 MiB")
	}
	typ := input.Type
	if typ == "" {
		typ = http.DetectContentType(input.Body)
	}
	if _, _, err := mime.ParseMediaType(typ); err != nil {
		return nil, errors.New("invalid: attachment media type")
	}
	filename := roomAttachmentFilename(input.Filename)
	commit := func(commitCtx context.Context, tx *sql.Tx, entry blob.Blob, created bool) error {
		if !created {
			var scoped int
			if err := tx.QueryRowContext(commitCtx, "SELECT COUNT(*) FROM room_attachments WHERE sha256=?", entry.SHA256).Scan(&scoped); err != nil {
				return err
			}
			if scoped == 0 {
				return errors.New("conflict: this file already exists without room restrictions; share its existing link")
			}
		}
		var currentEvent string
		if err := tx.QueryRowContext(commitCtx, "SELECT event_id FROM rooms WHERE id=? AND deleted_at=0", roomID).Scan(&currentEvent); err != nil {
			return err
		}
		if currentEvent != room.EventID {
			return errors.New("conflict: the room changed while uploading; try again")
		}
		_, err := tx.ExecContext(commitCtx, `INSERT OR IGNORE INTO room_attachments(room_id,room_event_id,sha256,filename,created_at) VALUES(?,?,?,?,unixepoch())`, roomID, room.EventID, entry.SHA256, filename)
		return err
	}
	entry, err := t.blobs.Put(ctx, blob.PutOptions{Reader: bytes.NewReader(input.Body), Type: typ, Uploader: actor, Commit: commit})
	if err != nil {
		return nil, err
	}
	descriptor := t.blobs.Descriptor(strings.TrimRight(t.publicURL, "/")+"/media", entry)
	descriptor["filename"] = filename
	descriptor["room"] = roomID
	descriptor["nip94"] = append(descriptor["nip94"].([][]string), []string{"filename", filename})
	return descriptor, nil
}

func roomAttachmentFilename(filename string) string {
	filename = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, path.Base(filename))
	runes := []rune(strings.TrimSpace(filename))
	if len(runes) > 255 {
		runes = runes[:255]
	}
	filename = string(runes)
	if filename == "" || filename == "." || filename == "/" {
		return "file"
	}
	return filename
}

func (t *Tenant) roomAttachmentUploader(ctx context.Context, actor, roomID string) error {
	if actor == "" {
		return errors.New("auth-required: attachment uploader")
	}
	if !t.Policy().Features.Files {
		return errors.New("restricted: file uploads are disabled")
	}
	banned, err := t.community.IsBanned(ctx, actor)
	if err != nil {
		return fmt.Errorf("check attachment uploader ban: %w", err)
	}
	if banned {
		return errors.New("blocked: this pubkey is banned from this relay")
	}
	standing, err := t.community.Role(ctx, actor)
	if err != nil {
		return fmt.Errorf("check attachment uploader role: %w", err)
	}
	allowed := t.Policy().Writes == "open" || (t.Policy().Writes == "owner" && standing == "owner") || (t.Policy().Writes != "owner" && standing != "")
	if !allowed {
		return errors.New("restricted: file upload is not allowed")
	}
	if standing == "agent" {
		if _, err := t.agentUploadGrant(ctx, actor); err != nil {
			return err
		}
	}
	if room, role, err := t.roomFor(ctx, actor, roomID); err != nil {
		return err
	} else if role == "" && !(room.Access == community.RoomOpen && standing != "") {
		return errors.New("restricted: join the room before uploading attachments")
	}
	return nil
}

func (t *Tenant) initRoomAttachments(ctx context.Context) error {
	_, err := t.store.DB().ExecContext(ctx, `CREATE TABLE IF NOT EXISTS room_attachments (
		room_id TEXT NOT NULL, room_event_id TEXT NOT NULL DEFAULT '',
		sha256 TEXT NOT NULL, filename TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL, PRIMARY KEY(room_id, room_event_id, sha256),
		FOREIGN KEY(sha256) REFERENCES blobs(sha256) ON DELETE CASCADE
	); CREATE INDEX IF NOT EXISTS room_attachments_hash ON room_attachments(sha256);`)
	if err != nil {
		return fmt.Errorf("initialize room attachments: %w", err)
	}
	return nil
}

func (t *Tenant) roomAttachmentRooms(ctx context.Context, hash string) ([]roomAttachmentScope, error) {
	rows, err := t.store.DB().QueryContext(ctx, "SELECT room_id,room_event_id FROM room_attachments WHERE sha256=?", hash)
	if err != nil {
		return nil, fmt.Errorf("lookup room attachment: %w", err)
	}
	defer rows.Close()
	var rooms []roomAttachmentScope
	for rows.Next() {
		var room roomAttachmentScope
		if err := rows.Scan(&room.RoomID, &room.EventID); err != nil {
			return nil, fmt.Errorf("scan room attachment: %w", err)
		}
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

func (t *Tenant) roomAttachmentAccess(ctx context.Context, actor, hash string) error {
	rooms, err := t.roomAttachmentRooms(ctx, hash)
	if err != nil {
		return err
	}
	if len(rooms) == 0 {
		return nil
	}
	for _, scope := range rooms {
		if room, _, err := t.roomFor(ctx, actor, scope.RoomID); err == nil && room.EventID == scope.EventID {
			return nil
		}
	}
	if actor == "" {
		return errors.New("auth-required: room attachment")
	}
	return errors.New("restricted: room attachment membership required")
}

func (t *Tenant) roomAttachmentHTTP(w http.ResponseWriter, r *http.Request) bool {
	if match := roomAttachmentPath.FindStringSubmatch(r.URL.Path); match != nil {
		if r.Method == http.MethodPut {
			t.uploadRoomAttachment(w, r, match[1])
		} else {
			w.Header().Set("Allow", http.MethodPut)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return true
	}
	match := mediaPath.FindStringSubmatch(r.URL.Path)
	if match == nil {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	if err := t.serveRoomMedia(w, r, match[1]); err != nil {
		browseHTTPError(w, err)
	}
	return true
}

func (t *Tenant) serveRoomMedia(w http.ResponseWriter, r *http.Request, hash string) error {
	var actor string
	var err error
	if auth.IsBlossomAuthorization(r.Header.Get("Authorization")) {
		actor, err = t.authorizeBlob(r, blob.ActionGet)
	} else {
		actor, err = t.resolveUIActor(r)
	}
	if err != nil {
		return err
	}
	if err := t.browseRead(r.Context(), actor); err != nil {
		return err
	}
	// Use the same safe content headers and range serving as /files/raw.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	request := r.Clone(r.Context())
	query := request.URL.Query()
	query.Set("hash", hash)
	request.URL.RawQuery = query.Encode()
	return t.fileDownload(w, request, actor)
}

func (t *Tenant) uploadRoomAttachment(w http.ResponseWriter, r *http.Request, roomID string) {
	// Authenticate and authorize before buffering file bytes. The payload
	// binding is checked after reading, without consuming the proof twice.
	token, err := t.auth.VerifyNIP98(r.Header.Get("Authorization"), t.requestURL(r), r.Method, "")
	if err != nil {
		browseHTTPError(w, err)
		return
	}
	if err := t.roomAttachmentUploader(r.Context(), token.PubKey, roomID); err != nil {
		browseHTTPError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, roomAttachmentMaxBytes+1)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "attachment exceeds 32 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	if int64(len(body)) > roomAttachmentMaxBytes {
		http.Error(w, "attachment exceeds 32 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	hash := sha256.Sum256(body)
	if err := t.auth.ValidateBlobPayload(r.Header.Get("Authorization"), hex.EncodeToString(hash[:])); err != nil {
		browseHTTPError(w, err)
		return
	}
	result, err := t.storeRoomAttachment(r.Context(), token.PubKey, roomID, roomAttachmentInput{Body: body, Type: r.Header.Get("Content-Type"), Filename: r.URL.Query().Get("filename")})
	if err != nil {
		browseHTTPError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
