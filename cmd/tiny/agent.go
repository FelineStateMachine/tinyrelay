package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/agentrunner"
)

func agent(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tiny agent", flag.ContinueOnError)
	fs.SetOutput(out)
	relay := fs.String("relay", "", "Nostr relay WebSocket URL")
	command := fs.String("command", "", "agent executable")
	protocol := fs.String("protocol", "acp", "agent protocol: acp or command (text stdin/stdout)")
	keyEnv := fs.String("key-env", "TINY_AGENT_KEY", "environment variable containing the agent hex key or nsec")
	state := fs.String("state", "", "queue journal path (required)")
	cwd := fs.String("cwd", ".", "agent working directory")
	timeout := fs.Duration("timeout", 30*time.Minute, "maximum task and approval wait")
	var rooms, commandArgs []string
	fs.Func("room", "authorized room id (repeatable)", func(value string) error {
		if !regexp.MustCompile(`^[a-z0-9_-]{1,64}$`).MatchString(value) {
			return errors.New("invalid room id")
		}
		rooms = append(rooms, value)
		return nil
	})
	fs.Func("arg", "argument passed directly to the agent (repeatable)", func(value string) error { commandArgs = append(commandArgs, value); return nil })
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *relay == "" || *command == "" || *state == "" || len(rooms) == 0 {
		return errors.New("agent requires --relay, --room, --command and --state")
	}
	if fs.NArg() != 0 || (*protocol != "acp" && *protocol != "command") || *timeout <= 0 {
		return errors.New("invalid agent options")
	}
	secret, err := secretKey(os.Getenv(*keyEnv))
	if err != nil {
		return fmt.Errorf("agent key: %w", err)
	}
	directory, err := filepath.Abs(*cwd)
	if err != nil {
		return err
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return errors.New("agent working directory does not exist")
	}
	// The subprocess does not need the relay signing key. It receives messages
	// through stdio and returns requests and output to this signing boundary.
	environment := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, *keyEnv+"=") {
			environment = append(environment, entry)
		}
	}
	log := func(err error) { fmt.Fprintln(out, err) }
	return agentrunner.Serve(ctx, agentrunner.ServiceOptions{RelayURL: *relay, Secret: secret, Rooms: rooms, StatePath: *state, Timeout: *timeout, Log: log, Process: agentrunner.ProcessOptions{Command: *command, Args: commandArgs, Env: environment, CWD: directory, Protocol: *protocol}})
}
