package blob

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DetectMediaType recognises images, video, audio and PDF from the first
// bytes of a blob. Anything else stays an octet stream so uploads can never
// be reinterpreted as a page or a script.
func DetectMediaType(head []byte) string {
	detected := contentType(http.DetectContentType(head))
	for _, prefix := range []string{"image/", "video/", "audio/"} {
		if strings.HasPrefix(detected, prefix) {
			return detected
		}
	}
	if detected == "application/pdf" {
		return detected
	}
	return "application/octet-stream"
}

func detectFileType(name string) string {
	file, err := os.Open(name)
	if err != nil {
		return "application/octet-stream"
	}
	defer file.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	return DetectMediaType(head[:n])
}

func contentType(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(strings.Split(raw, ";")[0]))
	if raw == "" {
		return "application/octet-stream"
	}
	return raw
}
func extensionSuffix(typ string) string {
	if ext := extensions[typ]; ext != "" {
		return "." + ext
	}
	return ""
}

func typeForExtension(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "application/octet-stream"
	}
	name := strings.ToLower(filepath.Ext(u.Path))
	for typ, ext := range extensions {
		if "."+ext == name {
			return typ
		}
	}
	return "application/octet-stream"
}
func contentTypes() []string {
	out := make([]string, 0, len(extensions))
	for typ := range extensions {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}

func parsePageCount(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 20
	}
	if value > 100 {
		return 100
	}
	return value
}

func parsePage(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func parseListLimit(raw string) (int, error) {
	if raw == "" {
		return 20, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 100 {
		return 0, errors.New("invalid: limit must be between 1 and 100")
	}
	return value, nil
}
func chooseStatus(created bool, status int) int {
	if created {
		return status
	}
	return http.StatusOK
}
func statusFor(err error) int {
	if errors.Is(err, ErrHashMismatch) {
		return http.StatusConflict
	}
	if errors.Is(err, ErrMultipartConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, ErrFileTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	if errors.Is(err, ErrQuotaExceeded) {
		return http.StatusForbidden
	}
	if errors.Is(err, ErrBlocked) || errors.Is(err, ErrNotEncrypted) {
		return http.StatusForbidden
	}
	if strings.HasPrefix(err.Error(), "auth-required:") {
		return http.StatusUnauthorized
	}
	if strings.HasPrefix(err.Error(), "invalid:") {
		return http.StatusBadRequest
	}
	if strings.HasPrefix(err.Error(), "restricted:") {
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok || value == "" {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func validateMirrorURL(raw string, resolve ResolveIP) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("invalid: only https urls can be mirrored")
	}
	ips, err := resolve(context.Background(), u.Hostname())
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

func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return false
		}
		if ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0 {
			return false
		}
		if ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19) {
			return false
		}
	}
	return true
}
func (s *Service) safeClient() *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	transport := base.Clone()
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := s.resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, errors.New("origin has no reachable public address")
	}
	return &http.Client{Timeout: 5 * time.Minute, Transport: transport, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		return validateMirrorURL(req.URL.String(), s.resolve)
	}}
}
