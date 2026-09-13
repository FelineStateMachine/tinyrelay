// Package relaycmd contains the shared standalone relay command runner.
package relaycmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/tinyrelay"
)

const (
	defaultMessageBytes = 1 << 20
	defaultPendingBytes = 4 << 20
)

type options struct {
	dataDir, listen, publicURL string
	name, description, contact string
	owner                      string
	authRequired               bool
	maxMessageBytes            int64
	maxPendingBytes            int
}

func parse(args []string) (options, error) {
	return parseArgs(args, io.Discard)
}

func parseArgs(args []string, output io.Writer) (options, error) {
	opts := options{dataDir: defaultDataDir(), listen: "127.0.0.1:7447", maxMessageBytes: defaultMessageBytes, maxPendingBytes: defaultPendingBytes}
	fs := flag.NewFlagSet("tinyrelay", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&opts.dataDir, "data-dir", opts.dataDir, "relay data directory")
	fs.StringVar(&opts.listen, "listen", opts.listen, "HTTP/WebSocket listen address")
	fs.StringVar(&opts.publicURL, "public-url", "", "public HTTP(S) or WS(S) relay URL")
	fs.StringVar(&opts.name, "name", "", "relay name")
	fs.StringVar(&opts.description, "description", "", "relay description")
	fs.StringVar(&opts.contact, "contact", "", "relay operator contact")
	fs.StringVar(&opts.owner, "owner", "", "relay owner lowercase hex public key")
	fs.BoolVar(&opts.authRequired, "auth-required", false, "require AUTH before reading and publishing as the authenticated author")
	fs.Int64Var(&opts.maxMessageBytes, "max-message-bytes", opts.maxMessageBytes, "maximum websocket message size")
	fs.IntVar(&opts.maxPendingBytes, "max-pending-bytes", opts.maxPendingBytes, "maximum queued output per connection")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, errors.New("tinyrelay accepts flags only")
	}
	var err error
	if opts.publicURL, err = normalizeRelayURL(opts.publicURL); err != nil {
		return opts, err
	}
	if opts.maxMessageBytes <= 0 || opts.maxPendingBytes <= 0 {
		return opts, errors.New("message and pending byte limits must be positive")
	}
	if opts.owner != "" && !validOwner(opts.owner) {
		return opts, errors.New("owner must be a lowercase 64-character hex public key")
	}
	return opts, nil
}

func defaultDataDir() string {
	if value := strings.TrimSpace(os.Getenv("TINY_RELAY_DATA_DIR")); value != "" {
		return value
	}
	return "tinyrelay-data"
}

func normalizeRelayURL(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("public URL must be an HTTP(S) or WS(S) URL without query or fragment")
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", errors.New("public URL must use http, https, ws or wss")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func validOwner(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

// Run serves a standalone, protocol-compatible relay until ctx is canceled.
func Run(ctx context.Context, args []string, output io.Writer) error {
	opts, err := parseArgs(args, output)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", opts.listen, err)
	}
	defer listener.Close()
	publicURL := opts.publicURL
	if publicURL == "" {
		publicURL = "ws://" + listener.Addr().String()
	}
	app, err := tinyrelay.OpenServer(ctx, tinyrelay.ServerConfig{
		DataDir: opts.dataDir, PublicURL: publicURL, Name: opts.name,
		Description: opts.description, Contact: opts.contact, Owner: opts.owner,
		AuthRequired: opts.authRequired, MaxMessageBytes: opts.maxMessageBytes,
		MaxPendingBytes: opts.maxPendingBytes,
	})
	if err != nil {
		return err
	}
	server := &http.Server{Handler: app, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	if _, err := fmt.Fprintf(output, "tinyrelay listening on %s\n", listener.Addr()); err != nil {
		_ = server.Close()
		serveErr := <-done
		return errors.Join(err, joinServeErrors(serveErr, app.Close()))
	}
	select {
	case serveErr := <-done:
		closeErr := app.Close()
		return joinServeErrors(serveErr, closeErr)
	case <-ctx.Done():
		return stopServer(server, app, done)
	}
}

func stopServer(server *http.Server, app *tinyrelay.Server, done <-chan error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = server.Close()
	}
	serveErr := <-done
	closeErr := app.Close()
	return errors.Join(shutdownErr, joinServeErrors(serveErr, closeErr))
}

func joinServeErrors(serveErr, closeErr error) error {
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, closeErr)
}
