package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/daemon"
	"github.com/FelineStateMachine/tinyrelay/internal/templates"
)

const version = "0.1.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tiny:", err)
		os.Exit(1)
	}
}

// run dispatches arguments after the executable name and writes command output
// to out. Returning errors to main keeps process exit at the command boundary.
func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, "tiny serve [--data-dir PATH] [--listen :7447]\ntiny tenant create --name NAME --owner PUBKEY [--template default]\ntiny tenant list|enable|disable|host [options]\ntiny git-token --repo URL [--key-env TINY_AGENT_KEY] [--format header|value|git]\ntiny templates\ntiny version")
		return err
	}
	switch args[0] {
	case "version":
		_, err := fmt.Fprintln(out, "tiny", version)
		return err
	case "templates":
		return json.NewEncoder(out).Encode(templates.Names())
	case "serve":
		return serve(ctx, args[1:], out)
	case "tenant":
		return tenantCommand(ctx, args[1:], out)
	case "git-token":
		return gitToken(args[1:], out)
	default:
		return fmt.Errorf("unknown command %q; run tiny help", args[0])
	}
}

func defaultDataDir() string {
	if value := os.Getenv("TINY_DATA_DIR"); value != "" {
		return value
	}
	if value := os.Getenv("XDG_DATA_HOME"); value != "" {
		return filepath.Join(value, "tiny")
	}
	return "data"
}

func tenantCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("tenant subcommand required")
	}
	fs := flag.NewFlagSet("tiny tenant "+args[0], flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "local data directory")
	name := fs.String("name", "", "tenant name")
	owner := fs.String("owner", "", "owner public key (64 lowercase hex characters)")
	templateName := fs.String("template", "default", "relay template")
	source := fs.String("source", "", "upstream relay for a replica template")
	host := fs.String("host", "", "custom hostname")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	app, err := daemon.New(ctx, daemon.Config{DataDir: *dataDir, Version: version})
	if err != nil {
		return err
	}
	defer func() {
		if err := app.Close(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "close tenant catalog:", err)
		}
	}()
	if args[0] == "create" {
		tenant, err := app.Create(ctx, daemon.CreateOptions{Name: *name, Owner: *owner, Template: *templateName, Source: *source})
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(tenant)
	}
	if args[0] == "list" {
		tenants, err := app.Catalog().List(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(tenants)
	}
	meta, err := app.Catalog().GetByName(ctx, *name)
	if err != nil {
		return err
	}
	switch args[0] {
	case "enable":
		err = app.Catalog().Enable(ctx, meta.ID)
	case "disable":
		err = app.Catalog().Disable(ctx, meta.ID)
	case "host":
		if *host == "" {
			err = app.Catalog().ClearHost(ctx, meta.ID)
		} else {
			err = app.Catalog().SetHost(ctx, meta.ID, *host)
		}
	default:
		return fmt.Errorf("unknown tenant command %q", args[0])
	}
	if err != nil {
		return err
	}
	meta, err = app.Catalog().GetByID(ctx, meta.ID)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(meta)
}

type serveOptions struct {
	cfg               daemon.Config
	listen            string
	diagnostics       string
	peers             []string
	peerInterval      time.Duration
	peerTimeout       time.Duration
	peerPublish       bool
	peerPublishTenant string
}

func parseServe(args []string) (serveOptions, error) {
	var opts serveOptions
	fs := flag.NewFlagSet("tiny serve", flag.ContinueOnError)
	fs.StringVar(&opts.cfg.DataDir, "data-dir", defaultDataDir(), "local data directory")
	fs.StringVar(&opts.listen, "listen", ":7447", "public HTTP listener")
	fs.StringVar(&opts.cfg.PublicURL, "public-url", os.Getenv("TINY_PUBLIC_URL"), "external https URL (reverse proxy)")
	fs.StringVar(&opts.cfg.DefaultTenant, "default-tenant", "main", "tenant served on the root path")
	fs.StringVar(&opts.diagnostics, "diagnostics", "", "separate diagnostics listener, e.g.127.0.0.1:9090")
	fs.StringVar(&opts.cfg.Telemetry.DebugToken, "diagnostics-token", os.Getenv("TINY_DIAGNOSTICS_TOKEN"), "diagnostics bearer token")
	fs.StringVar(&opts.cfg.Telemetry.OTLPEndpoint, "otlp-endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), "optional OTLP HTTP trace export")
	fs.Float64Var(&opts.cfg.Telemetry.TraceSampleRatio, "trace-sample", 0.01, "trace sampling ratio")
	fs.Int64Var(&opts.cfg.MaxMessageBytes, "max-message-bytes", 0, "optional host input byte guard; 0 unlimited")
	fs.BoolVar(&opts.cfg.AllowPrivateRelays, "allow-private-relays", false, "allow outbound relay connections to localhost, LAN, and Tailscale addresses")
	fs.Func("provision-owner", "operator-approved pubkey allowed to create tenants through the browser (repeatable)", func(value string) error {
		value = strings.TrimSpace(value)
		if len(value) != 64 || value != strings.ToLower(value) || !hexOwner(value) {
			return errors.New("provision owner must be a lowercase 64-character hex pubkey")
		}
		opts.cfg.ProvisionOwners = append(opts.cfg.ProvisionOwners, value)
		return nil
	})
	fs.Func("discovery-relay", "relay used to discover NIP-65 and inbox lists (repeatable)", func(value string) error {
		if !strings.HasPrefix(value, "ws://") && !strings.HasPrefix(value, "wss://") {
			return errors.New("discovery relay must use ws:// or wss://")
		}
		opts.cfg.DiscoveryRelays = append(opts.cfg.DiscoveryRelays, value)
		return nil
	})
	fs.Func("push-callback-origin", "operator-approved HTTPS callback origin (repeatable)", func(value string) error {
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("callback origin must be https://hostname[:port]")
		}
		opts.cfg.PushCallbackOrigins = append(opts.cfg.PushCallbackOrigins, "https://"+strings.ToLower(u.Host))
		return nil
	})
	fs.IntVar(&opts.cfg.MaxPendingBytes, "max-pending-bytes", 4<<20, "connection output backpressure guard")
	fs.Func("peer", "configured NIP-66 peer URL to probe (repeatable)", func(value string) error {
		opts.peers = append(opts.peers, value)
		return nil
	})
	fs.DurationVar(&opts.peerInterval, "peer-interval", time.Minute, "NIP-66 peer probe interval")
	fs.DurationVar(&opts.peerTimeout, "peer-timeout", 5*time.Second, "NIP-66 peer probe timeout")
	fs.BoolVar(&opts.peerPublish, "peer-publish", false, "publish signed NIP-66 status for the explicitly selected tenant")
	fs.StringVar(&opts.peerPublishTenant, "peer-publish-tenant", "", "tenant whose relay identity publishes NIP-66 status (required with --peer-publish)")
	opts.cfg.Version = version
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if opts.diagnostics != "" && strings.TrimSpace(opts.cfg.Telemetry.DebugToken) == "" {
		return opts, errors.New("diagnostics listener requires --diagnostics-token or TINY_DIAGNOSTICS_TOKEN")
	}
	if opts.peerPublish && strings.TrimSpace(opts.peerPublishTenant) == "" {
		return opts, errors.New("--peer-publish requires --peer-publish-tenant")
	}
	if opts.peerPublish && len(opts.peers) == 0 {
		return opts, errors.New("--peer-publish requires at least one --peer peer URL")
	}
	if len(opts.peers) > 0 {
		opts.cfg.PeerMonitor = &daemon.PeerMonitorConfig{Peers: opts.peers, Interval: opts.peerInterval, Timeout: opts.peerTimeout, Publish: opts.peerPublish}
	}
	return opts, nil
}

func hexOwner(value string) bool {
	for _, r := range value {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
}

// serve owns listeners and peer monitoring around the App lifecycle. Shutdown
// drains the servers and joins peer monitoring before deferred App cleanup.
func serve(ctx context.Context, args []string, out io.Writer) error {
	opts, err := parseServe(args)
	if err != nil {
		return err
	}
	app, err := daemon.New(ctx, opts.cfg)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := app.Close(closeCtx); err != nil {
			fmt.Fprintln(os.Stderr, "close daemon:", err)
		}
	}()
	listener, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	baseURL := opts.cfg.PublicURL
	if baseURL == "" {
		host, port, err := net.SplitHostPort(listener.Addr().String())
		if err != nil {
			return errors.Join(err, listener.Close())
		}
		if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
			host = "127.0.0.1"
		}
		baseURL = "http://" + net.JoinHostPort(host, port)
	}
	if err := app.Start(ctx, baseURL); err != nil {
		return errors.Join(err, listener.Close())
	}
	if opts.peerPublish {
		if err := app.ConfigurePeerPublication(ctx, opts.peerPublishTenant); err != nil {
			return errors.Join(err, listener.Close())
		}
	}
	peerCtx, stopPeer := context.WithCancel(ctx)
	defer stopPeer()
	peerDone := make(chan error, 1)
	if app.PeerMonitor() != nil {
		go func() { peerDone <- app.PeerMonitorRun(peerCtx) }()
	} else {
		close(peerDone)
	}
	server := &http.Server{Handler: app, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	servers := []*http.Server{server}
	results := make(chan error, 1)
	go func() { results <- server.Serve(listener) }()
	if opts.diagnostics != "" {
		diag, err := net.Listen("tcp", opts.diagnostics)
		if err != nil {
			closeErr := server.Close()
			return errors.Join(err, closeErr)
		}
		debugServer := &http.Server{Handler: app.Diagnostics(), ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, debugServer)
		go func() { results <- debugServer.Serve(diag) }()
	}
	if _, err := fmt.Fprintf(out, "tiny listening on %s; data in %s\n", listener.Addr(), opts.cfg.DataDir); err != nil {
		for _, srv := range servers {
			if closeErr := srv.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
		for range servers {
			<-results
		}
		return err
	}
	serverErr := awaitServers(ctx, servers, results)
	stopPeer()
	if app.PeerMonitor() != nil {
		if err := <-peerDone; err != nil && !errors.Is(err, context.Canceled) {
			serverErr = errors.Join(serverErr, err)
		}
	}
	return serverErr
}

// awaitServers starts shutdown when ctx ends or a server returns. Each server
// must report once to results; all reports are drained before returning the
// joined shutdown and serving errors.
func awaitServers(ctx context.Context, servers []*http.Server, results <-chan error) error {
	var first error
	received := 0
	select {
	case <-ctx.Done():
	case first = <-results:
		received = 1
	}
	if errors.Is(first, http.ErrServerClosed) {
		first = nil
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdown); err != nil {
			first = errors.Join(first, err, server.Close())
		}
	}
	for received < len(servers) {
		err := <-results
		received++
		if !errors.Is(err, http.ErrServerClosed) {
			first = errors.Join(first, err)
		}
	}
	return first
}
