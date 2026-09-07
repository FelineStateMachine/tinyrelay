package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

func (t *Tenant) privateHTTPAuth(ctx context.Context, method, rawURL, payloadHash string) (string, error) {
	if t == nil || t.records == nil || !t.PrivateServiceEnabled() {
		return "", fmt.Errorf("private peer: relay identity unavailable")
	}
	if !privatePeerMatch(rawURL, t.Policy().PrivatePeers) {
		return "", fmt.Errorf("private peer: target is no longer configured")
	}
	proof, err := t.records.SignNIP98(ctx, method, rawURL, payloadHash, 0)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return "", err
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(raw), nil
}

func privatePeerMatch(source string, peers []string) bool {
	u, err := url.Parse(source)
	if err != nil {
		return false
	}
	for _, peer := range peers {
		p, err := url.Parse(strings.TrimRight(strings.TrimSpace(peer), "/"))
		if err != nil || normalizePeerScheme(p.Scheme) != normalizePeerScheme(u.Scheme) || p.Host != u.Host {
			continue
		}
		base := strings.TrimRight(p.Path, "/")
		if base == "" || u.Path == base || strings.HasPrefix(u.Path, base+"/") {
			return true
		}
	}
	return false
}

func (t *Tenant) privatePeerReady(ctx context.Context, target string) bool {
	if !t.PrivateServiceEnabled() || !privatePeerMatch(target, t.Policy().PrivatePeers) {
		return false
	}
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	if u.Scheme == "ws" {
		u.Scheme = "http"
	} else if u.Scheme == "wss" {
		u.Scheme = "https"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/nostr+json")
	res, err := privatePeerHTTPClient(t.app.cfg.AllowPrivateRelays).Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return false
	}
	var info struct {
		Supported []string `json:"supported_grasps"`
		Private   bool     `json:"private_service"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&info); err != nil {
		return false
	}
	for _, profile := range info.Supported {
		if strings.EqualFold(profile, "GRASP-08") {
			return true
		}
	}
	return false
}

func privatePeerHTTPClient(allowPrivate bool) *http.Client {
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
			if !allowPrivate && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
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

func normalizePeerScheme(scheme string) string {
	if scheme == "ws" {
		return "http"
	}
	if scheme == "wss" {
		return "https"
	}
	return scheme
}
