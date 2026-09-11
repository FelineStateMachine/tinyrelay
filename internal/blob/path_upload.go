package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// maxPathUploadSources bounds the remote sources one BUD-13 upload may name.
const maxPathUploadSources = 8

// pathUpload implements the draft BUD-13 path-addressed upload endpoint.
// The route itself is dispatched by serveHTTP after validating the hash path.
func (s *Service) pathUpload(w http.ResponseWriter, r *http.Request, expectedHash string) {
	// Authenticate before inspecting the body. This is important for signed
	// uploads: an invalid request must not consume a potentially large stream.
	pubkey, err := s.authorize(r, ActionUpload)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, err)
		return
	}

	urls := r.URL.Query()["url"]
	if len(urls) == 0 {
		entry, created, putErr := s.putValidatedWithMetadata(r.Context(), r.Body,
			contentType(r.Header.Get("content-type")), pubkey, expectedHash,
			r.URL.Query().Get("filename"), r.URL.Query().Get("path"), r.URL.Query().Get("access"), r.URL.Query().Get("purpose"),
			func(actual string) error {
				if s.config.ValidateUpload != nil {
					return s.config.ValidateUpload(r, actual)
				}
				return nil
			}, nil)
		if putErr != nil {
			s.fail(w, statusFor(putErr), putErr)
			return
		}
		s.writeJSON(w, chooseStatus(created, http.StatusCreated), s.descriptor(r, entry))
		return
	}

	// BUD-13 remote uploads must have an empty request body. Check this after
	// authorization, including chunked requests whose ContentLength is unknown.
	if r.Body != nil {
		var probe [1]byte
		n, readErr := io.ReadFull(r.Body, probe[:])
		if n != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
			s.fail(w, http.StatusBadRequest, errors.New("invalid: remote upload body must be empty"))
			return
		}
	}
	if len(urls) > maxPathUploadSources {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("invalid: at most %d remote sources per upload", maxPathUploadSources))
		return
	}
	for _, raw := range urls {
		if err := s.validatePathUploadURL(r.Context(), raw); err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
	}

	client := s.config.HTTPClient
	if client == nil {
		client = s.safeClient()
	}
	var mismatch bool
	for _, raw := range urls {
		fetchRequest, requestErr := http.NewRequestWithContext(r.Context(), http.MethodGet, raw, nil)
		if requestErr != nil {
			continue
		}
		response, fetchErr := client.Do(fetchRequest)
		if fetchErr != nil {
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_ = response.Body.Close()
			continue
		}
		typ := contentType(response.Header.Get("content-type"))
		if typ == "application/octet-stream" {
			typ = typeForExtension(raw)
		}
		entry, created, putErr := s.putValidatedWithMetadata(r.Context(), response.Body, typ,
			pubkey, expectedHash, "", "", r.URL.Query().Get("access"), r.URL.Query().Get("purpose"), func(actual string) error {
				if s.config.ValidateUpload != nil {
					return s.config.ValidateUpload(r, actual)
				}
				return nil
			}, nil)
		_ = response.Body.Close()
		if putErr == nil {
			s.writeJSON(w, chooseStatus(created, http.StatusCreated), s.descriptor(r, entry))
			return
		}
		if errors.Is(putErr, ErrHashMismatch) {
			mismatch = true
			continue
		}
		s.fail(w, statusFor(putErr), putErr)
		return
	}
	if mismatch {
		s.fail(w, http.StatusConflict, ErrHashMismatch)
		return
	}
	s.fail(w, http.StatusBadGateway, errors.New("error: no usable remote source"))
}

func (s *Service) validatePathUploadURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid: url must be an absolute http or https url")
	}
	ips, err := s.resolve(ctx, u.Hostname())
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
