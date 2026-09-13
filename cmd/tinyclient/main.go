// Command tinyclient runs the standalone, server-rendered relay website.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/FelineStateMachine/tinyrelay/tinyclient"
)

func configure(args []string, output io.Writer) (*http.Server, error) {
	flags := flag.NewFlagSet("tinyclient", flag.ContinueOnError)
	flags.SetOutput(output)
	listen := flags.String("listen", "127.0.0.1:8081", "HTTP listen address")
	backend := flags.String("backend", "", "Upstream tinyrelay backend HTTP URL, including /r/<tenant> when used")
	public := flags.String("public-url", "", "Browser-facing HTTP(S) URL; preserve the relay's public origin and tenant path")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, errors.New("tinyclient accepts flags only")
	}
	handler, err := tinyclient.NewRemote(tinyclient.RemoteOptions{BackendURL: *backend, PublicURL: *public})
	if err != nil {
		return nil, err
	}
	return &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}, nil
}

func run(ctx context.Context, args []string, output io.Writer) error {
	server, err := configure(args, output)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	fmt.Fprintf(output, "tinyclient listening on %s\n", listener.Addr())
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-done:
		}
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "tinyclient:", err)
		os.Exit(1)
	}
}
