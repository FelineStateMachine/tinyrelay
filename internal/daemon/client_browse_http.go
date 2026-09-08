package daemon

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
)

func (t *Tenant) tryBrowseHTTP(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/repo/raw" && r.URL.Path != "/files/raw" {
		return false
	}
	ctx, finish := t.app.telemetry.Start(r.Context(), "browse-download")
	r = r.WithContext(ctx)
	outcome := "error"
	defer func() { finish(outcome) }()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", 405)
		return true
	}
	actor, err := t.resolveUIActor(r)
	if err == nil {
		err = t.browseRead(r.Context(), actor)
	}
	if err != nil {
		browseHTTPError(w, err)
		return true
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/repo/raw" {
		err = t.sourceDownload(w, r, actor)
	} else {
		err = t.fileDownload(w, r)
	}
	if err != nil {
		browseHTTPError(w, err)
	} else {
		outcome = "success"
	}
	return true
}

func browseHTTPError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case strings.HasPrefix(err.Error(), "auth-required:"):
		status = http.StatusUnauthorized
	case strings.HasPrefix(err.Error(), "restricted:"), strings.HasPrefix(err.Error(), "blocked:"):
		status = http.StatusForbidden
	case errors.Is(err, os.ErrNotExist), strings.HasPrefix(err.Error(), "not found:"):
		status = http.StatusNotFound
	}
	http.Error(w, err.Error(), status)
}

func (t *Tenant) sourceDownload(w http.ResponseWriter, r *http.Request, actor string) error {
	if t.git == nil {
		return os.ErrNotExist
	}
	values := r.URL.Query()
	q := gitrelay.BrowseRequest{Owner: values.Get("owner"), Repo: values.Get("repo"), Ref: values.Get("ref"), Path: values.Get("path")}
	repo, err := t.git.BrowseRepository(q.Owner, q.Repo)
	if err != nil {
		return err
	}
	if err := t.browseRepoAccess(r.Context(), actor, repo); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(q.Path)}))
	if r.Method == http.MethodHead {
		q.View = "file"
		_, err = t.git.Browse(r.Context(), q)
		return err
	}
	return t.git.WriteSource(r.Context(), q, w)
}

// mediaInline reports whether a stored type is safe to render in the browser.
func mediaInline(typ string) bool {
	return strings.HasPrefix(typ, "image/") || strings.HasPrefix(typ, "video/") || strings.HasPrefix(typ, "audio/") || typ == "application/pdf"
}

func (t *Tenant) fileDownload(w http.ResponseWriter, r *http.Request) error {
	if !t.Policy().Features.Files {
		return os.ErrNotExist
	}
	hash := r.URL.Query().Get("hash")
	entry, body, err := t.blobs.Get(r.Context(), hash)
	if err != nil {
		return err
	}
	defer body.Close()
	// Recognised media is served inline under its own type so the browser can
	// show it. Everything else downloads as an attachment.
	typ := entry.Type
	seeker, seekable := body.(io.ReadSeeker)
	if typ == "application/octet-stream" && seekable {
		head := make([]byte, 512)
		n, _ := io.ReadFull(seeker, head)
		typ = blob.DetectMediaType(head[:n])
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	if !mediaInline(typ) {
		typ = "application/octet-stream"
	}
	w.Header().Set("Content-Type", typ)
	if typ == "application/octet-stream" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": entry.SHA256}))
	} else {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": entry.SHA256}))
	}
	if seekable {
		http.ServeContent(w, r, entry.SHA256, time.Unix(entry.Uploaded, 0), seeker)
		return nil
	}
	if r.Method == http.MethodHead {
		return nil
	}
	_, err = io.Copy(w, body)
	return err
}
