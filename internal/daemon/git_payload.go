package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// A NIP-98 payload hash must match before Git can mutate the repository.
// Spooling keeps that ordering without allocating memory proportional to a
// pack's size. GitRelay closes the replacement body on every request exit.
func spoolGitPayload(r *http.Request, proof event.Event) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	file, err := os.CreateTemp("", "tiny-git-authorized-*")
	if err != nil {
		return fmt.Errorf("spool Git authorization: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), r.Body)
	if err != nil {
		return fmt.Errorf("read Git authorization payload: %w", err)
	}
	if n == 0 {
		return nil
	}
	if event.Tag(proof, "payload") != hex.EncodeToString(hash.Sum(nil)) {
		return errors.New("auth-required: token payload hash does not match the body")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind Git authorization payload: %w", err)
	}
	r.Body = &gitPayloadBody{File: file, original: r.Body}
	keep = true
	return nil
}

type gitPayloadBody struct {
	*os.File
	original io.ReadCloser
	once     sync.Once
	err      error
}

func (b *gitPayloadBody) Close() error {
	b.once.Do(func() {
		b.err = errors.Join(b.File.Close(), os.Remove(b.Name()), b.original.Close())
	})
	return b.err
}
