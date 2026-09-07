package gitrelay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// HTTPAuthSigner signs one complete HTTP request. The payload hash is empty
// for requests without a body. It is deliberately a callback so GitRelay
// never owns or handles a tenant's private key.
type HTTPAuthSigner func(context.Context, string, string, string) (string, error)

// ConfigurePrivatePeers refreshes operator-controlled private transport after
// a tenant policy update without reopening the Git store.
func (g *GitRelay) ConfigurePrivatePeers(peers []string, signer HTTPAuthSigner) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.privatePeers = append([]string(nil), peers...)
	g.httpAuth = signer
}

type privateProxy struct {
	server   *http.Server
	listener net.Listener
	done     chan struct{}
	base     *url.URL
	token    string
	sign     HTTPAuthSigner
	addr     string
	client   *http.Client
}

func newPrivateProxy(ctx context.Context, target string, sign HTTPAuthSigner, allowPrivate ...bool) (*privateProxy, error) {
	base, err := url.Parse(target)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("Git private peer: invalid target")
	}
	if sign == nil {
		return nil, errors.New("Git private peer: missing signer")
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("Git private peer token: %w", err)
	}
	private := len(allowPrivate) > 0 && allowPrivate[0]
	p := &privateProxy{base: base, token: hex.EncodeToString(raw), sign: sign, done: make(chan struct{}), client: privateHTTPClient(private)}
	p.server = &http.Server{Handler: http.HandlerFunc(p.serve)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("Git private peer listener: %w", err)
	}
	p.listener = listener
	p.addr = listener.Addr().String()
	go func() { defer close(p.done); _ = p.server.Serve(listener) }()
	return p, nil
}

func (p *privateProxy) URL() string {
	return "http://" + p.addr + "/tiny/" + p.token
}

func (p *privateProxy) Close(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		ctx = context.Background()
	}
	err := p.server.Shutdown(ctx)
	if err != nil && p.listener != nil {
		_ = p.listener.Close()
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		if p.listener != nil {
			_ = p.listener.Close()
		}
		<-p.done
	}
	return err
}

func (p *privateProxy) serve(w http.ResponseWriter, r *http.Request) {
	prefix := "/tiny/" + p.token
	if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
		http.NotFound(w, r)
		return
	}
	u := *p.base
	suffix := strings.TrimPrefix(r.URL.Path, prefix)
	if strings.Contains(suffix, "..") {
		http.Error(w, "invalid private peer path", http.StatusBadRequest)
		return
	}
	u.Path = strings.TrimSuffix(p.base.Path, "/") + "/" + strings.TrimPrefix(suffix, "/")
	u.RawQuery = r.URL.RawQuery
	file, hash, size, err := spoolRequest(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if file != nil {
		defer func() { _ = os.Remove(file.Name()); _ = file.Close() }()
	}
	payload := ""
	if size > 0 {
		payload = hex.EncodeToString(hash[:])
	}
	authorization, err := p.sign(r.Context(), r.Method, u.String(), payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var body io.Reader
	if file != nil {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		body = file
	}
	request, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(request.Header, r.Header)
	request.Header.Set("Authorization", authorization)
	request.ContentLength = size
	response, err := p.client.Do(request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func spoolRequest(body io.ReadCloser) (*os.File, [32]byte, int64, error) {
	var digest [32]byte
	if body == nil || body == http.NoBody {
		return nil, digest, 0, nil
	}
	file, err := os.CreateTemp("", "tiny-private-git-")
	if err != nil {
		return nil, digest, 0, err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), body)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, digest, 0, err
	}
	copy(digest[:], hash.Sum(nil))
	return file, digest, size, nil
}

func copyHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Transfer-Encoding") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func privateHTTPClient(allowPrivate bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !allowPrivate && privateIP(ip) {
				continue
			}
			conn, dialErr := (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
		}
		return nil, fmt.Errorf("private peer: no usable address for %s", host)
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (g *GitRelay) privatePeer(source string) bool {
	return g.privatePeerBase(source) != ""
}

func (g *GitRelay) privatePeerBase(source string) string {
	u, err := url.Parse(source)
	if err != nil {
		return ""
	}
	for _, configured := range g.privatePeers {
		peer, err := url.Parse(strings.TrimRight(strings.TrimSpace(configured), "/"))
		if err != nil || normalizePeerScheme(peer.Scheme) != normalizePeerScheme(u.Scheme) || peer.Host != u.Host {
			continue
		}
		base := strings.TrimRight(peer.Path, "/")
		if base == "" || u.Path == base || strings.HasPrefix(u.Path, base+"/") {
			return strings.TrimRight(peer.String(), "/")
		}
	}
	return ""
}

func probePrivatePeer(ctx context.Context, source string, allowPrivate ...bool) error {
	u, err := url.Parse(source)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/nostr+json")
	private := len(allowPrivate) > 0 && allowPrivate[0]
	res, err := privateHTTPClient(private).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("private peer NIP-11 status %d", res.StatusCode)
	}
	var info struct {
		SupportedGRASPs []string `json:"supported_grasps"`
		PrivateService  bool     `json:"private_service"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&info); err != nil {
		return err
	}
	for _, profile := range info.SupportedGRASPs {
		if strings.EqualFold(profile, "GRASP-08") {
			return nil
		}
	}
	return errors.New("private peer does not advertise GRASP-08")
}

func normalizePeerScheme(scheme string) string {
	if scheme == "ws" {
		return "http"
	}
	if scheme == "wss" {
		return "https"
	}
	return scheme
}
