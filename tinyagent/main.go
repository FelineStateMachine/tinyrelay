package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
	"github.com/FelineStateMachine/tinyrelay/tinyagent/client"
)

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}
type response struct {
	ID           json.RawMessage `json:"id,omitempty"`
	Result       any             `json:"result,omitempty"`
	Error        *rpcError       `json:"error,omitempty"`
	Event        string          `json:"event,omitempty"`
	Subscription string          `json:"subscription,omitempty"`
	Data         any             `json:"data,omitempty"`
}
type rpcError struct {
	Message string `json:"message"`
}
type rpcServer struct {
	client *client.Client
	outMu  sync.Mutex
	subMu  sync.Mutex
	subs   map[string]context.CancelFunc
	sem    chan struct{}
	wg     sync.WaitGroup
	subWG  sync.WaitGroup
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string, in io.Reader, out, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: tinyagent keygen | rpc | call")
	}
	switch args[0] {
	case "keygen":
		return keygen(out)
	case "rpc":
		return rpc(args[1:], in, out)
	case "call":
		return call(args[1:], out)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
func keygen(out io.Writer) error {
	secret, err := nostr.GenerateKey()
	if err != nil {
		return err
	}
	pub, err := nostr.PublicKey(secret)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]string{"secret": secret, "pubkey": pub})
}
func rpc(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("rpc", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	relay := fs.String("relay", "", "relay URL")
	keyEnv := fs.String("key-env", "TINY_PRIVATE_KEY", "private key environment variable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *relay == "" {
		return errors.New("--relay is required")
	}
	secret := os.Getenv(*keyEnv)
	if secret == "" {
		return fmt.Errorf("%s is not set", *keyEnv)
	}
	c, err := client.New(*relay, secret)
	if err != nil {
		return err
	}
	s := &rpcServer{client: c, subs: make(map[string]context.CancelFunc), sem: make(chan struct{}, 32)}
	return s.serve(in, out)
}
func (s *rpcServer) serve(in io.Reader, out io.Writer) error {
	scan := bufio.NewScanner(in)
	// A 32 MiB binary upload expands to roughly 43 MiB in base64, plus JSON
	// framing and the request envelope.
	scan.Buffer(make([]byte, 4096), 48<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for scan.Scan() {
		var req request
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			s.write(out, response{Error: &rpcError{Message: "invalid request: " + err.Error()}})
			continue
		}
		if req.Method != "subscribe" {
			s.sem <- struct{}{}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if req.Method != "subscribe" {
				defer func() { <-s.sem }()
			}
			s.handle(ctx, req, out)
		}()
	}
	s.wg.Wait()
	s.stopAll()
	s.subWG.Wait()
	return scan.Err()
}
func (s *rpcServer) handle(parent context.Context, req request, out io.Writer) {
	if req.Method == "subscribe" {
		if err := s.launchSubscribe(parent, req.Params, out); err != nil {
			s.write(out, response{ID: req.ID, Error: &rpcError{Message: err.Error()}})
			return
		}
		s.write(out, response{ID: req.ID, Result: map[string]bool{"subscribed": true}})
		return
	}
	ctx := parent
	cancel := func() {}
	if req.Method != "subscribe" {
		ctx, cancel = context.WithTimeout(parent, 2*time.Minute)
	}
	defer cancel()
	result, err := s.dispatch(ctx, req.Method, req.Params, out)
	if err != nil {
		s.write(out, response{ID: req.ID, Error: &rpcError{Message: err.Error()}})
		return
	}
	s.write(out, response{ID: req.ID, Result: result})
}
func (s *rpcServer) dispatch(ctx context.Context, method string, raw json.RawMessage, out io.Writer) (any, error) {
	var p map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
	}
	switch method {
	case "identity":
		return s.client.Identity(), nil
	case "publish":
		var e nostr.Event
		if err := json.Unmarshal(p["event"], &e); err != nil {
			return nil, err
		}
		return s.client.Publish(ctx, e)
	case "query":
		var f []client.Filter
		if err := json.Unmarshal(p["filter"], &f); err != nil {
			var one client.Filter
			if objectErr := json.Unmarshal(p["filter"], &one); objectErr != nil {
				return nil, err
			}
			f = []client.Filter{one}
		}
		return s.client.Query(ctx, f)
	case "request":
		var path, method string
		if err := json.Unmarshal(p["path"], &path); err != nil || path == "" {
			return nil, errors.New("path is required")
		}
		if len(p["method"]) > 0 {
			if err := json.Unmarshal(p["method"], &method); err != nil {
				return nil, err
			}
		}
		return s.client.Request(ctx, path, method, p["body"])
	case "upload":
		var room, filename, typ string
		if err := json.Unmarshal(p["room"], &room); err != nil || room == "" {
			return nil, errors.New("room is required")
		}
		if len(p["filename"]) > 0 {
			if err := json.Unmarshal(p["filename"], &filename); err != nil {
				return nil, err
			}
		}
		if len(p["type"]) > 0 {
			if err := json.Unmarshal(p["type"], &typ); err != nil {
				return nil, err
			}
		}
		var encoded string
		if err := json.Unmarshal(p["data"], &encoded); err != nil {
			return nil, err
		}
		data, err := decode(encoded)
		if err != nil {
			return nil, err
		}
		return s.client.Upload(ctx, room, filename, typ, data)
	case "download":
		var path string
		if err := json.Unmarshal(p["path"], &path); err != nil {
			return nil, err
		}
		return s.client.Download(ctx, path)
	case "unsubscribe":
		var name string
		if err := json.Unmarshal(p["subscription"], &name); err != nil {
			return nil, err
		}
		s.subMu.Lock()
		stop := s.subs[name]
		delete(s.subs, name)
		s.subMu.Unlock()
		if stop != nil {
			stop()
		}
		return map[string]bool{"stopped": stop != nil}, nil
	default:
		return nil, fmt.Errorf("unknown method %q", method)
	}
}
func (s *rpcServer) launchSubscribe(parent context.Context, raw json.RawMessage, out io.Writer) error {
	var p map[string]json.RawMessage
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	var name string
	if err := json.Unmarshal(p["subscription"], &name); err != nil || name == "" {
		return errors.New("subscription is required")
	}
	var f client.Filter
	if err := json.Unmarshal(p["filter"], &f); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	s.subMu.Lock()
	previous := s.subs[name]
	s.subs[name] = cancel
	s.subMu.Unlock()
	if previous != nil {
		previous()
	}
	s.subWG.Add(1)
	go func() {
		defer s.subWG.Done()
		err := s.client.Subscribe(ctx, name, f, func(kind string, e nostr.Event) {
			if kind == "connected" {
				s.write(out, response{Event: kind, Subscription: name})
				return
			}
			s.write(out, response{Event: "event", Subscription: kind, Data: e})
		})
		if err != nil && parent.Err() == nil {
			s.write(out, response{Event: "error", Subscription: name, Data: map[string]string{"message": err.Error()}})
		}
		// Keep the cancellation handle until an explicit unsubscribe. A newer
		// subscription may already own this name when the old stream exits.
	}()
	return nil
}
func (s *rpcServer) subscribe(parent context.Context, p map[string]json.RawMessage, out io.Writer) error {
	var name string
	if err := json.Unmarshal(p["subscription"], &name); err != nil || name == "" {
		return errors.New("subscription is required")
	}
	var f client.Filter
	if err := json.Unmarshal(p["filter"], &f); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	s.subMu.Lock()
	s.subs[name] = cancel
	s.subMu.Unlock()
	return s.client.Subscribe(ctx, name, f, func(kind string, e nostr.Event) {
		if kind == "connected" {
			s.write(out, response{Event: kind, Subscription: name})
			return
		}
		s.write(out, response{Event: "event", Subscription: kind, Data: e})
	})
}
func (s *rpcServer) stopAll() {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for name, stop := range s.subs {
		stop()
		delete(s.subs, name)
	}
}
func (s *rpcServer) write(out io.Writer, v response) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_ = json.NewEncoder(out).Encode(v)
}
func decode(raw string) ([]byte, error) {
	const max = 32 << 20
	if len(raw) > max*2 {
		return nil, errors.New("invalid attachment data")
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(b) > max {
		return nil, errors.New("invalid attachment data")
	}
	return b, nil
}
func call(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	relay := fs.String("relay", "", "relay URL")
	keyEnv := fs.String("key-env", "TINY_PRIVATE_KEY", "private key environment variable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *relay == "" || fs.NArg() < 1 {
		return errors.New("usage: tinyagent call --relay URL METHOD [JSON_PARAMS]")
	}
	secret := os.Getenv(*keyEnv)
	if secret == "" {
		return fmt.Errorf("%s is not set", *keyEnv)
	}
	c, err := client.New(*relay, secret)
	if err != nil {
		return err
	}
	method := fs.Arg(0)
	raw := json.RawMessage(`{}`)
	if fs.NArg() > 1 {
		raw = json.RawMessage(fs.Arg(1))
	}
	server := &rpcServer{client: c, subs: make(map[string]context.CancelFunc), sem: make(chan struct{}, 1)}
	result, err := server.dispatch(context.Background(), method, raw, out)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
