package gitrelay

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

var errRepairByteLimit = errors.New("PR repair transfer exceeds byte limit")

// A single budget covers every connection, ref and source in one repair.
// Counting tunneled TLS bytes also bounds HTTPS without terminating its TLS.
type byteBudget struct{ used atomic.Int64 }

func (b *byteBudget) take(wanted, limit int64) int64 {
	for {
		old := b.used.Load()
		allowed := min(wanted, max(0, limit-old))
		if b.used.CompareAndSwap(old, old+allowed) {
			return allowed
		}
	}
}

type repairProxy struct {
	listener net.Listener
	// credential is the Proxy-Authorization value Git presents. The listener
	// is loopback only, but other local processes cannot borrow the tunnel.
	credential  string
	url         string
	ctx         context.Context
	cancel      context.CancelFunc
	budget      *byteBudget
	limit       int64
	targetIP    string
	targetHost  string
	targetPort  string
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closeOnce   sync.Once
}

func newRepairProxy(ctx context.Context, targetIP, targetHost, targetPort string, budget *byteBudget, limit int64) (*repairProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var secret [16]byte
	if _, err := rand.Read(secret[:]); err != nil {
		_ = listener.Close()
		return nil, err
	}
	proxyCtx, cancel := context.WithCancel(ctx)
	token := hex.EncodeToString(secret[:])
	credential := "Basic " + base64.StdEncoding.EncodeToString([]byte("tiny:"+token))
	p := &repairProxy{listener: listener, credential: credential, ctx: proxyCtx, cancel: cancel, budget: budget, limit: limit, targetIP: targetIP, targetHost: targetHost, targetPort: targetPort, connections: make(map[net.Conn]struct{})}
	p.url = "http://tiny:" + token + "@" + listener.Addr().String()
	context.AfterFunc(proxyCtx, func() { _ = p.Close() })
	go p.serve()
	return p, nil
}

func (p *repairProxy) URL() string { return p.url }

func (p *repairProxy) Close() error {
	p.closeOnce.Do(func() {
		p.cancel()
		_ = p.listener.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for conn := range p.connections {
			_ = conn.Close()
		}
	})
	return nil
}

func (p *repairProxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		_ = conn.Close()
		return false
	}
	p.connections[conn] = struct{}{}
	return true
}

func (p *repairProxy) release(conn net.Conn) {
	_ = conn.Close()
	p.mu.Lock()
	delete(p.connections, conn)
	p.mu.Unlock()
}

func (p *repairProxy) serve() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		if p.track(conn) {
			go p.handle(conn)
		}
	}
}

func (p *repairProxy) handle(client net.Conn) {
	defer p.release(client)
	reader := bufio.NewReader(client)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if request.Method != http.MethodConnect && request.Method != http.MethodGet && request.Method != http.MethodPost {
		return
	}
	if subtle.ConstantTimeCompare([]byte(request.Header.Get("Proxy-Authorization")), []byte(p.credential)) != 1 {
		_, _ = io.WriteString(client, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"tiny\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	host := request.URL.Hostname()
	port := request.URL.Port()
	if port == "" {
		port = "80"
		if request.Method == http.MethodConnect || request.URL.Scheme == "https" {
			port = "443"
		}
	}
	if !strings.EqualFold(host, p.targetHost) || port != p.targetPort {
		return
	}
	if p.budget.used.Load() >= p.limit {
		return
	}
	// Dial only the already validated address. The proxy never resolves a
	// host supplied by the client and never follows HTTP redirects.
	upstream, err := (&net.Dialer{}).DialContext(p.ctx, "tcp", net.JoinHostPort(p.targetIP, p.targetPort))
	if err != nil {
		return
	}
	if !p.track(upstream) {
		return
	}
	defer p.release(upstream)
	if request.Method == http.MethodConnect {
		if _, err := io.WriteString(repairBudgetWriter{p, client}, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(repairBudgetWriter{p, client}, upstream); done <- struct{}{} }()
		// Keep bytes already buffered while reading CONNECT (e.g., a TLS
		// handshake sent in the same write as the request).
		go func() { _, _ = io.Copy(repairBudgetWriter{p, upstream}, reader); done <- struct{}{} }()
		select {
		case <-done:
		case <-p.ctx.Done():
		}
		return
	}
	request.RequestURI = ""
	request.Close = true
	if err := request.Write(repairBudgetWriter{p, upstream}); err != nil {
		return
	}
	_, _ = io.Copy(repairBudgetWriter{p, client}, upstream)
}

type repairBudgetWriter struct {
	proxy *repairProxy
	dst   io.Writer
}

func (w repairBudgetWriter) Write(data []byte) (int, error) {
	allowed := w.proxy.budget.take(int64(len(data)), w.proxy.limit)
	n, err := w.dst.Write(data[:allowed])
	if err == nil && allowed < int64(len(data)) {
		err = errRepairByteLimit
	}
	return n, err
}
