package sites

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// Blob is a fetched site file and its declared content metadata.
type Blob struct {
	Body io.ReadCloser
	Type string
	Size int64
}

// BlobGet opens a local blob by its SHA-256 hash. The caller closes Blob.Body.
type BlobGet func(ctx context.Context, blobSHA string) (Blob, error)

// BlobPut stores bytes under blobSHA with the supplied content type. The
// callback consumes the reader before returning.
type BlobPut func(ctx context.Context, blobSHA string, contentType string, body io.Reader) error

// Fetch performs an outbound site file request. The caller closes the
// returned response body.
type Fetch func(ctx context.Context, request *http.Request) (*http.Response, error)

// ResolveIP resolves a host before an outbound site request is allowed.
type ResolveIP func(ctx context.Context, host string) ([]net.IP, error)

// ResolveHost resolves a request host to a site label.
type ResolveHost func(ctx context.Context, host string) (label string, err error)

// ReadAccess authorizes a request for site content.
type ReadAccess func(request *http.Request) bool

// Config supplies storage, blob, network and request policy dependencies.
type Config struct {
	Store         *storage.Store
	GetBlob       BlobGet
	PutBlob       BlobPut
	Fetch         Fetch
	ResolveIP     ResolveIP
	MaxFetchBytes int64
	Policy        func() policy.Policy
	ReadAccess    ReadAccess
	BaseDomain    string
	ResolveHost   ResolveHost
	RelayURL      func(*http.Request) string
}

type Service struct{ config Config }

// Mirror copies every missing manifest file from its advertised servers into
// the local blob store. It is safe to retry after a partial failure.
func (s *Service) Mirror(ctx context.Context, manifest event.Event) error {
	if err := ValidateManifest(manifest); err != nil {
		return err
	}
	if !s.config.Policy().Features.Sites.Mirror {
		return nil
	}
	if s.config.PutBlob == nil {
		return errors.New("site mirror requires a blob writer")
	}
	for _, mapping := range SitePaths(manifest) {
		if s.config.GetBlob != nil {
			if local, err := s.config.GetBlob(ctx, mapping[2]); err == nil {
				if local.Body != nil {
					_ = local.Body.Close()
				}
				continue
			}
		}
		blob, err := s.blob(ctx, manifest, mapping)
		if err != nil {
			return fmt.Errorf("mirror %s: %w", mapping[1], err)
		}
		data, err := readVerified(blob.Body, mapping[2], s.config.MaxFetchBytes)
		_ = blob.Body.Close()
		if err != nil {
			return fmt.Errorf("verify %s: %w", mapping[1], err)
		}
		if err := s.config.PutBlob(ctx, mapping[2], chooseType(blob.Type, mapping[1]), bytes.NewReader(data)); err != nil {
			return fmt.Errorf("store %s: %w", mapping[1], err)
		}
	}
	return nil
}

// RunMirrorIntent resolves the event captured by storage.AddIntents and runs
// the idempotent mirror operation. Unknown intent kinds are rejected so a
// worker cannot accidentally execute a different work queue entry here.
func (s *Service) RunMirrorIntent(ctx context.Context, intent storage.Intent) error {
	if intent.Kind != "site-mirror" {
		return fmt.Errorf("unsupported site intent %q", intent.Kind)
	}
	result, err := s.config.Store.Query(ctx, event.Filter{IDs: []string{intent.EventID}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		return err
	}
	if len(result.Events) == 0 {
		return nil
	}
	return s.Mirror(ctx, result.Events[0])
}

func New(config Config) (*Service, error) {
	if config.Store == nil {
		return nil, errors.New("sites store is required")
	}
	if config.Policy == nil {
		config.Policy = func() policy.Policy { return policy.Defaults("") }
	}
	if config.BaseDomain == "" {
		config.BaseDomain = "localhost"
	}
	if config.ResolveIP == nil {
		config.ResolveIP = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	if config.MaxFetchBytes < 0 {
		return nil, errors.New("sites maximum fetch bytes cannot be negative")
	}
	if err := ensureManifestSchema(context.Background(), config.Store.DB()); err != nil {
		return nil, fmt.Errorf("sites schema: %w", err)
	}
	return &Service{config: config}, nil
}

func (s *Service) SaveOptions(e event.Event, now int64) storage.SaveOptions {
	if e.Kind != KindSite && e.Kind != KindNamedSite && e.Kind != KindSiteSnapshot {
		return storage.SaveOptions{Now: now}
	}
	if err := ValidateManifest(e); err != nil {
		return storage.SaveOptions{Now: now}
	}
	opts := storage.SaveOptions{Now: now, BeforeCommit: func(ctx context.Context, tx *sql.Tx) error { return s.applyManifest(ctx, tx, e) }}
	if s.config.Policy().Features.Sites.Mirror {
		opts.Intents = []storage.Intent{{Kind: "site-mirror", EventID: e.ID, Payload: SiteLabel(e)}}
	}
	return opts
}

func (s *Service) ApplyTx(ctx context.Context, tx *sql.Tx, e event.Event) error {
	return s.applyManifest(ctx, tx, e)
}

// DeleteTx removes a manifest index entry in the same transaction that
// removes its event. Callers should invoke this for deletion and replacement
// paths so routing never points at an event that is no longer visible.
func (s *Service) DeleteTx(ctx context.Context, tx *sql.Tx, eventID string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM site_manifests WHERE event_id=?", eventID)
	return err
}

func (s *Service) applyManifest(ctx context.Context, tx *sql.Tx, e event.Event) error {
	if e.Kind != KindSite && e.Kind != KindNamedSite && e.Kind != KindSiteSnapshot {
		return nil
	}
	label := SiteLabel(e)
	if label == "" {
		return errors.New("invalid: site label")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO site_manifests(label,event_id,kind,pubkey,d,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(label) DO UPDATE SET event_id=excluded.event_id,kind=excluded.kind,pubkey=excluded.pubkey,d=excluded.d,updated_at=excluded.updated_at`, label, e.ID, e.Kind, e.PubKey, event.Tag(e, "d"), time.Now().Unix())
	return err
}

func (s *Service) Manifest(ctx context.Context, site Site) (event.Event, error) {
	f := event.Filter{Kinds: []int{site.Kind}, Tags: map[string][]string{}}
	if site.Kind == KindSiteSnapshot {
		f.IDs = []string{site.ID}
	} else {
		f.Authors = []string{site.PubKey}
		if site.Kind == KindNamedSite {
			f.Tags["d"] = []string{site.D}
		}
	}
	result, err := s.config.Store.Query(ctx, f, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		return event.Event{}, err
	}
	if len(result.Events) == 0 {
		return event.Event{}, sql.ErrNoRows
	}
	if err := ValidateManifest(result.Events[0]); err != nil {
		return event.Event{}, err
	}
	return result.Events[0], nil
}

func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serve) }

// MatchesHost reports whether the daemon should dispatch this Host header to
// the site door before trying relay routes.
func (s *Service) MatchesHost(host string) bool {
	_, ok := s.labelForHost(context.Background(), host)
	return ok
}

// HandlerForLabel returns a handler bound to a catalog-resolved site label.
// It is useful when the daemon has already selected a tenant from its host
// registry and must prevent a request from crossing into another tenant.
func (s *Service) HandlerForLabel(label string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serveLabel(w, r, label) })
}

func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	label, ok := s.labelForHost(r.Context(), r.Host)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serveLabel(w, r, label)
}

func (s *Service) serveLabel(w http.ResponseWriter, r *http.Request, label string) {
	if !s.config.Policy().Features.Sites.Enabled {
		http.NotFound(w, r)
		return
	}
	if s.config.ReadAccess != nil && !s.config.ReadAccess(r) {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	site, ok := ParseSite(label)
	if !ok {
		http.NotFound(w, r)
		return
	}
	manifest, err := s.Manifest(r.Context(), site)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/.well-known/nostr.json" {
		if _, present := r.URL.Query()["path"]; present {
			s.discovery(w, r, manifest)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path, redirect, ok := sitePath(r.URL.EscapedPath(), SitePaths(manifest))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if redirect {
		location := *r.URL
		location.Path = path
		location.RawPath = ""
		location.Host = r.Host
		location.Scheme = "http"
		if r.TLS != nil {
			location.Scheme = "https"
		}
		http.Redirect(w, r, location.String(), http.StatusPermanentRedirect)
		return
	}
	if err := s.servePath(w, r, manifest, path); err != nil {
		http.NotFound(w, r)
	}
}

func (s *Service) discovery(w http.ResponseWriter, r *http.Request, manifest event.Event) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Headers", "authorization, content-type, accept")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	values, ok := r.URL.Query()["path"]
	if !ok || len(values) != 1 {
		writeDiscovery(w, http.StatusBadRequest, nil, r.Method == http.MethodHead)
		return
	}
	path, ok := RequestedPath(values[0])
	if !ok || !validDiscoveryPath(path) {
		writeDiscovery(w, http.StatusBadRequest, nil, r.Method == http.MethodHead)
		return
	}
	selected, redirect, valid := sitePath(path, SitePaths(manifest))
	if redirect {
		selected, _, valid = sitePath(path+"/", SitePaths(manifest))
	}
	if !valid || findPath(SitePaths(manifest), selected) == nil {
		writeDiscovery(w, http.StatusOK, nil, r.Method == http.MethodHead)
		return
	}
	filter := manifestFilter(manifest)
	relay := ""
	if s.config.RelayURL != nil {
		relay = s.config.RelayURL(r)
	}
	if relay == "" {
		relay = requestOrigin(r)
	}
	writeDiscovery(w, http.StatusOK, map[string]any{path: map[string]any{"filter": filter, "relays": []string{relay}}}, r.Method == http.MethodHead)
}

func manifestFilter(e event.Event) map[string]any {
	if e.Kind == KindSiteSnapshot {
		return map[string]any{"ids": []string{e.ID}, "limit": 1}
	}
	result := map[string]any{"authors": []string{e.PubKey}, "kinds": []int{e.Kind}, "limit": 1}
	if e.Kind == KindNamedSite {
		result["#d"] = []string{event.Tag(e, "d")}
	}
	return result
}

func validDiscoveryPath(path string) bool {
	if path == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." || part == "." {
			return false
		}
	}
	return true
}

func writeDiscovery(w http.ResponseWriter, status int, value any, head bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if value != nil && !head {
		raw, _ := json.Marshal(value)
		_, _ = w.Write(raw)
	}
}

func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Service) labelForHost(ctx context.Context, host string) (string, bool) {
	host = strings.ToLower(strings.Split(host, ":")[0])
	if s.config.ResolveHost != nil {
		label, err := s.config.ResolveHost(ctx, host)
		if err == nil && label != "" {
			if _, ok := ParseSite(label); ok {
				return label, true
			}
		}
	}
	suffix := "." + strings.ToLower(strings.TrimSuffix(s.config.BaseDomain, "."))
	if strings.HasSuffix(host, suffix) {
		label := strings.TrimSuffix(host, suffix)
		if _, ok := ParseSite(label); ok {
			return label, true
		}
	}
	for _, custom := range s.config.Policy().CustomHosts {
		if strings.EqualFold(custom.Host, host) && custom.Site != "" {
			return custom.Site, true
		}
	}
	return "", false
}

func sitePath(raw string, paths [][]string) (string, bool, bool) {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", false, false
	}
	directory := !strings.Contains(pathBase(decoded), ".")
	redirect := directory && !strings.HasSuffix(decoded, "/")
	if redirect {
		decoded += "/"
		raw += "/"
	}
	lookup := decoded
	if directory {
		lookup += "index.html"
	}
	for _, tag := range paths {
		if len(tag) == 3 && (tag[1] == raw || tag[1] == lookup) {
			if redirect {
				return decoded, true, true
			}
			return tag[1], redirect, true
		}
	}
	return lookup, redirect, true
}

func pathBase(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func (s *Service) servePath(w http.ResponseWriter, r *http.Request, manifest event.Event, path string) error {
	mapping := findPath(SitePaths(manifest), path)
	status := http.StatusOK
	if mapping == nil {
		mapping = findPath(SitePaths(manifest), "/404.html")
		status = http.StatusNotFound
	}
	if mapping == nil {
		return errors.New("site file not found")
	}
	blob, err := s.blob(r.Context(), manifest, mapping)
	if err != nil {
		return err
	}
	defer blob.Body.Close()
	data, err := readVerified(blob.Body, mapping[2], s.config.MaxFetchBytes)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", `"`+mapping[2]+`"`)
	w.Header().Set("Content-Type", chooseType(blob.Type, path))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	cache := "public, no-cache, must-revalidate, no-transform"
	if s.config.Policy().Reads != "open" {
		cache = "private, no-store, no-transform"
	}
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return nil
	}
	if r.Header.Get("If-None-Match") == `"`+mapping[2]+`"` && status == http.StatusOK {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	if status == http.StatusOK {
		http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(data))
		return nil
	}
	w.WriteHeader(status)
	_, err = w.Write(data)
	return err
}

func findPath(paths [][]string, wanted string) []string {
	for _, path := range paths {
		if len(path) == 3 && path[1] == wanted {
			return path
		}
	}
	return nil
}
func chooseType(blobType, path string) string {
	if blobType != "" {
		return blobType
	}
	return SiteType(path)
}
func readVerified(body io.Reader, want string, maxBytes int64) ([]byte, error) {
	limited := body
	if maxBytes > 0 {
		limited = io.LimitReader(body, maxBytes+1)
	}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, errors.New("site file exceeds fetch limit")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return nil, errors.New("site file hash mismatch")
	}
	return data, nil
}

func (s *Service) blob(ctx context.Context, manifest event.Event, mapping []string) (Blob, error) {
	if s.config.GetBlob != nil {
		if blob, err := s.config.GetBlob(ctx, mapping[2]); err == nil {
			return blob, nil
		}
	}
	if s.config.Fetch == nil {
		return Blob{}, errors.New("site file is not local")
	}
	for _, raw := range s.servers(ctx, manifest) {
		blob, err := s.fetchRemote(ctx, raw, mapping)
		if err == nil {
			return blob, nil
		}
	}
	return Blob{}, errors.New("site file unavailable")
}

func (s *Service) fetchRemote(ctx context.Context, raw string, mapping []string) (Blob, error) {
	u, err := url.Parse(raw)
	if err != nil || !safeOrigin(u) || !s.publicOrigin(ctx, u) {
		return Blob{}, errors.New("unsafe site origin")
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + mapping[2]
	u.RawQuery = ""
	var response *http.Response
	for redirects := 0; redirects <= 3; redirects++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return Blob{}, err
		}
		response, err = s.fetch(ctx, req)
		if err != nil {
			return Blob{}, err
		}
		if response == nil || response.Body == nil {
			return Blob{}, errors.New("empty origin response")
		}
		if response.StatusCode < 300 || response.StatusCode >= 400 {
			break
		}
		location := response.Header.Get("location")
		response.Body.Close()
		if location == "" {
			return Blob{}, errors.New("origin redirect has no location")
		}
		next, err := url.Parse(location)
		if err != nil || !safeOrigin(next) || !s.publicOrigin(ctx, next) {
			return Blob{}, errors.New("unsafe site redirect")
		}
		next.Path = strings.TrimSuffix(next.Path, "/") + "/" + mapping[2]
		next.RawQuery = ""
		u = next
		if redirects == 3 {
			return Blob{}, errors.New("too many origin redirects")
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return Blob{}, errors.New("origin response was not successful")
	}
	if s.config.MaxFetchBytes > 0 && response.ContentLength > s.config.MaxFetchBytes {
		response.Body.Close()
		return Blob{}, errors.New("site file exceeds operator fetch limit")
	}
	return Blob{Body: response.Body, Type: response.Header.Get("content-type"), Size: response.ContentLength}, nil
}

func (s *Service) fetch(ctx context.Context, req *http.Request) (*http.Response, error) {
	if s.config.Fetch != nil {
		return s.config.Fetch(ctx, req)
	}
	return s.pinnedClient(ctx, req.URL.Hostname()).Do(req)
}

func (s *Service) pinnedClient(ctx context.Context, hostname string) *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{ServerName: hostname, MinVersion: tls.VersionTLS12}, DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
		ips, err := s.config.ResolveIP(dialCtx, hostname)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("origin DNS lookup failed")
		}
		for _, ip := range ips {
			if !isPublicIP(ip) {
				return nil, errors.New("origin resolved to a private address")
			}
		}
		port := "443"
		if _, p, err := net.SplitHostPort(address); err == nil {
			port = p
		}
		return (&net.Dialer{}).DialContext(dialCtx, network, net.JoinHostPort(ips[0].String(), port))
	}}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func safeOrigin(u *url.URL) bool {
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || net.ParseIP(u.Hostname()) != nil {
		return false
	}
	return !strings.HasSuffix(u.Hostname(), ".local") && !strings.HasSuffix(u.Hostname(), ".internal")
}

func (s *Service) publicOrigin(ctx context.Context, u *url.URL) bool {
	ips, err := s.config.ResolveIP(ctx, u.Hostname())
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return false
		}
	}
	return true
}

func isPublicIP(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified()
}

func (s *Service) servers(ctx context.Context, manifest event.Event) []string {
	servers := append([]string(nil), event.TagValues(manifest, "server")...)
	result, err := s.config.Store.Query(ctx, event.Filter{Authors: []string{manifest.PubKey}, Kinds: []int{10063}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err == nil && len(result.Events) > 0 {
		servers = append(servers, event.TagValues(result.Events[0], "server")...)
	}
	seen := make(map[string]bool, len(servers))
	out := make([]string, 0, len(servers))
	for _, value := range servers {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
