package gitrelay

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ServeHTTP exposes Git smart HTTP at /npub/<identifier>.git and /prs/... .
// The subprocess boundary keeps pack parsing and object integrity in Git.
func (g *GitRelay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Git-Protocol, Authorization")
	if req.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	owner, id, suffix, alternative, ok := parsePathMode(req.URL.Path)
	if !ok {
		http.NotFound(w, req)
		return
	}
	r, err := g.lookup(owner, id)
	if alternative {
		g.mu.RLock()
		candidate, found := g.repos[key(owner, id)]
		g.mu.RUnlock()
		if found && candidate.Alternative {
			r, err = candidate, nil
		}
	}
	if err != nil {
		http.NotFound(w, req)
		return
	}
	if r.Private {
		// Private authorization may replace the body with a verified disk
		// spool. Close that replacement as well as the server-owned body.
		defer func() { _ = req.Body.Close() }()
		if g.authorizeHTTP == nil {
			http.Error(w, "private repository requires authorization", http.StatusForbidden)
			return
		}
		if err := g.authorizeHTTP(req.Context(), req, r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	if err := g.ensureRepo(r); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if suffix == "" && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><body><code>"+html.EscapeString(r.Owner+"/"+r.Identifier)+"</code></body></html>\n")
		return
	}
	if err := g.cgi(req, w, r, suffix); err != nil {
		var streamed *cgiStreamError
		if errors.As(err, &streamed) {
			slog.ErrorContext(req.Context(), "Git response stream failed", "error", err)
			// Close an incomplete pack, rather than mark it complete or append
			// an HTTP error page to Git's binary protocol.
			panic(http.ErrAbortHandler)
		}
		http.Error(w, err.Error(), 500)
		return
	}
	if req.Method == http.MethodPost {
		// The CGI response has already been committed. Promotion callbacks are
		// retried by the next scheduler pass; never corrupt a successful Git
		// receive-pack response with a late HTTP error.
		_ = g.PromotePending(req.Context(), r)
	}
}

func parsePath(path string) (owner, id, suffix string, ok bool) {
	owner, id, suffix, _, ok = parsePathMode(path)
	return
}

func parsePathMode(path string) (owner, id, suffix string, alternative, ok bool) {
	var err error
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return
	}
	if parts[0] == "prs" {
		if len(parts) < 3 {
			return
		}
		parts = parts[1:]
		alternative = true
	}
	if len(parts) > 0 && parts[0] == "npub" {
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return
	}
	if !strings.HasPrefix(parts[0], "npub1") || !strings.HasSuffix(parts[1], ".git") {
		return
	}
	owner, err = decodeNPub(parts[0])
	if err != nil {
		return
	}
	id, err = url.PathUnescape(strings.TrimSuffix(parts[1], ".git"))
	if err != nil || !validIdentifier(id) {
		return
	}
	if len(parts) > 2 {
		suffix = "/" + strings.Join(parts[2:], "/")
	}
	ok = true
	return
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func decodeNPub(s string) (string, error) {
	s = strings.ToLower(s)
	if !strings.HasPrefix(s, "npub1") {
		return "", errors.New("not an npub")
	}
	// Bech32 uses the first separator; the payload may itself contain the
	// character "1".
	pos := strings.IndexByte(s, '1')
	if pos < len("npub") || len(s)-pos < 7 {
		return "", errors.New("invalid npub")
	}
	data := make([]byte, 0, len(s)-pos-1)
	for _, c := range s[pos+1:] {
		i := strings.IndexRune(bech32Charset, c)
		if i < 0 {
			return "", errors.New("invalid npub character")
		}
		data = append(data, byte(i))
	}
	if bech32Polymod(append(hrpExpand(s[:pos]), data...)) != 1 {
		return "", errors.New("invalid npub checksum")
	}
	payload, err := convertBits(data[:len(data)-6], 5, 8, false)
	if err != nil || len(payload) != 32 {
		return "", errors.New("invalid npub payload")
	}
	return fmt.Sprintf("%x", payload), nil
}

func hrpExpand(s string) []byte {
	out := make([]byte, 0, len(s)*2+1)
	for _, c := range s {
		out = append(out, byte(c>>5))
	}
	out = append(out, 0)
	for _, c := range s {
		out = append(out, byte(c&31))
	}
	return out
}
func bech32Polymod(v []byte) uint64 {
	const g0 = 0x3b6a57b2
	const g1 = 0x26508e6d
	const g2 = 0x1ea119fa
	const g3 = 0x3d4233dd
	const g4 = 0x2a1462b3
	chk := uint64(1)
	for _, x := range v {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 | uint64(x)
		if top&1 != 0 {
			chk ^= g0
		}
		if top&2 != 0 {
			chk ^= g1
		}
		if top&4 != 0 {
			chk ^= g2
		}
		if top&8 != 0 {
			chk ^= g3
		}
		if top&16 != 0 {
			chk ^= g4
		}
	}
	return chk
}
func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	acc := uint(0)
	bits := uint(0)
	maxv := uint((1 << to) - 1)
	maxacc := uint((1 << (from + to - 1)) - 1)
	out := make([]byte, 0)
	for _, v := range data {
		if uint(v)>>from != 0 {
			return nil, errors.New("invalid bech32 data")
		}
		acc = (acc<<from | uint(v)) & maxacc
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte((acc>>bits)&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte((acc<<(to-bits))&maxv))
		}
	} else if bits >= from || byte((acc<<(to-bits))&maxv) != 0 {
		return nil, errors.New("invalid bech32 padding")
	}
	return out, nil
}

func (g *GitRelay) cgi(req *http.Request, w http.ResponseWriter, r Repository, suffix string) error {
	prefix := ""
	if r.Alternative {
		prefix = "/prs"
	}
	pathInfo := prefix + "/" + r.Owner + "/" + r.Identifier + ".git" + suffix
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	// Browser clients such as gitworkshop.dev fetch one commit at a time with
	// a filter, and refuse servers that do not advertise filter and
	// sha1-in-want. The -c settings reach upload-pack through the environment.
	cmd := exec.CommandContext(ctx, "git", "-c", "uploadpack.allowFilter=true", "-c", "uploadpack.allowAnySHA1InWant=true", "http-backend")
	cmd.Dir = g.root
	cmd.WaitDelay = 5 * time.Second
	stdin, contentLength, cleanup, err := cgiInput(req)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdin = stdin
	var stderr cgiDiagnostic
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer stdout.Close()
	query := req.URL.RawQuery
	env := os.Environ()
	env = append(env, "GIT_PROJECT_ROOT="+g.root, "GIT_HTTP_EXPORT_ALL=1", "PATH_INFO="+pathInfo, "PATH_TRANSLATED="+filepath.Join(g.root, strings.TrimPrefix(pathInfo, "/")), "REQUEST_METHOD="+req.Method, "QUERY_STRING="+query, "REMOTE_ADDR="+req.RemoteAddr)
	if value := req.Header.Get("Content-Type"); value != "" {
		env = append(env, "CONTENT_TYPE="+value)
	}
	if contentLength != "" {
		env = append(env, "CONTENT_LENGTH="+contentLength)
	}
	if value := req.Header.Get("Git-Protocol"); value != "" {
		env = append(env, "HTTP_GIT_PROTOCOL="+value)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git http-backend: %w", err)
	}
	controller := http.NewResponseController(w)
	_ = controller.EnableFullDuplex()
	reader := bufio.NewReader(stdout)
	started, streamErr := streamCGI(w, controller, reader)
	if streamErr != nil {
		cancel()
		_ = stdout.Close()
		_ = req.Body.Close()
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		streamErr = errors.Join(streamErr, fmt.Errorf("git http-backend: %w: %s", waitErr, strings.TrimSpace(stderr.String())))
	}
	if streamErr != nil && started {
		return &cgiStreamError{streamErr}
	}
	return streamErr
}

// Compressed smart-HTTP negotiation needs a decoded CONTENT_LENGTH for CGI.
// Spool this uncommon request form to disk so memory stays independent of size.
func cgiInput(req *http.Request) (io.Reader, string, func(), error) {
	length := req.Header.Get("Content-Length")
	if !strings.EqualFold(req.Header.Get("Content-Encoding"), "gzip") {
		return req.Body, length, func() {}, nil
	}
	zr, err := gzip.NewReader(req.Body)
	if err != nil {
		return nil, "", nil, fmt.Errorf("decode compressed Git request: %w", err)
	}
	defer zr.Close()
	file, err := os.CreateTemp("", "tiny-git-request-*")
	if err != nil {
		return nil, "", nil, err
	}
	cleanup := func() { _ = file.Close(); _ = os.Remove(file.Name()) }
	n, err := io.Copy(file, zr)
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("spool compressed Git request: %w", err)
	}
	return file, strconv.FormatInt(n, 10), cleanup, nil
}

type cgiStreamError struct{ error }

// Only diagnostic text is bounded; pack streams have no product size limit.
type cgiDiagnostic struct{ bytes.Buffer }

func (b *cgiDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 64*1024 - b.Len(); remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(remaining, n)])
	}
	return n, nil
}

func streamCGI(w http.ResponseWriter, controller *http.ResponseController, reader *bufio.Reader) (bool, error) {
	status := http.StatusOK
	responseHeaders := make(http.Header)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return false, fmt.Errorf("read Git response headers: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return false, errors.New("malformed Git CGI response header")
		}
		if strings.EqualFold(k, "Status") {
			if _, err := fmt.Sscanf(v, "%d", &status); err != nil || status < 200 || status > 599 {
				return false, errors.New("invalid Git CGI response status")
			}
		} else {
			responseHeaders.Add(k, strings.TrimSpace(v))
		}
	}
	// Hold the response until git produces its first body byte. HTTP/1.1
	// proxies without full duplex discard the unread request body once a
	// response starts, which would truncate a pack that is still uploading.
	// git-http-backend writes its headers before reading the request, but
	// writes body bytes only after it has consumed the pack.
	if _, err := reader.Peek(1); err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read Git response body: %w", err)
	}
	for k, values := range responseHeaders {
		for _, value := range values {
			w.Header().Add(k, value)
		}
	}
	w.WriteHeader(status)
	if err := controller.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return true, err
	}
	_, err := io.Copy(cgiFlushWriter{w, controller}, reader)
	return true, err
}

type cgiFlushWriter struct {
	writer     io.Writer
	controller *http.ResponseController
}

func (w cgiFlushWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil {
		err = w.controller.Flush()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	return n, err
}
