package agentrunner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type PermissionOption struct {
	ID   string `json:"optionId"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}
type Permission struct {
	Title   string
	Details string
	Options []PermissionOption
}
type ProcessOptions struct {
	Command    string
	Args       []string
	Env        []string
	CWD        string
	Protocol   string
	Progress   func(string)
	Permission func(context.Context, Permission) (string, error)
}
type rpcFrame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type rpcProcess struct {
	input      io.WriteCloser
	frames     chan rpcFrame
	finished   chan struct{}
	readErr    error
	writeMu    sync.Mutex
	cancelOnce sync.Once
	sessionMu  sync.RWMutex
	opts       ProcessOptions
	session    string
	text       strings.Builder
}

func RunProcess(ctx context.Context, opts ProcessOptions, prompt string) (string, error) {
	if opts.Command == "" {
		return "", errors.New("agent command is required")
	}
	if opts.Protocol == "command" {
		return runCommand(ctx, opts, prompt)
	}
	if opts.Protocol != "acp" {
		return "", errors.New("agent protocol must be acp or command")
	}
	command := exec.CommandContext(ctx, opts.Command, opts.Args...)
	configureProcessGroup(command)
	var process *rpcProcess
	command.Cancel = func() error {
		if process != nil && process.hasSession() {
			process.cancelSession()
		}
		terminateProcess(command)
		return nil
	}
	command.Dir = opts.CWD
	command.Env = opts.Env
	command.WaitDelay = 2 * time.Second
	input, err := command.StdinPipe()
	if err != nil {
		return "", err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	// Agent stderr belongs to the local runner, never to a room transcript.
	command.Stderr = io.Discard
	p := &rpcProcess{input: input, frames: make(chan rpcFrame, 1), finished: make(chan struct{}), opts: opts}
	process = p
	if err := command.Start(); err != nil {
		return "", fmt.Errorf("start agent: %w", err)
	}
	readCtx, stopRead := context.WithCancel(ctx)
	go p.read(readCtx, output)
	defer func() { stopRead(); input.Close(); terminateProcess(command); command.Wait(); <-p.finished }()
	init, err := p.call(ctx, 1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]string{"name": "tiny", "version": "1"}})
	if err != nil {
		return "", err
	}
	var initialized struct {
		Version int `json:"protocolVersion"`
	}
	if json.Unmarshal(init, &initialized) != nil || initialized.Version != 1 {
		return "", errors.New("agent does not support ACP version 1")
	}
	raw, err := p.call(ctx, 2, "session/new", map[string]any{"cwd": opts.CWD, "mcpServers": []any{}})
	if err != nil {
		return "", err
	}
	var session struct {
		ID string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &session) != nil || session.ID == "" {
		return "", errors.New("agent returned no session id")
	}
	p.setSession(session.ID)
	raw, err = p.call(ctx, 3, "session/prompt", map[string]any{"sessionId": session.ID, "prompt": []any{map[string]string{"type": "text", "text": prompt}}})
	if err != nil {
		if ctx.Err() != nil {
			p.cancelSession()
		}
		return "", err
	}
	var done struct {
		Reason string `json:"stopReason"`
	}
	if json.Unmarshal(raw, &done) != nil {
		return "", errors.New("invalid ACP prompt result")
	}
	if done.Reason == "cancelled" {
		return "", context.Canceled
	}
	return strings.TrimSpace(p.text.String()), nil
}
func (p *rpcProcess) read(ctx context.Context, out io.Reader) {
	defer close(p.finished)
	defer close(p.frames)
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var frame rpcFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			p.readErr = fmt.Errorf("invalid ACP response: %w", err)
			return
		}
		select {
		case p.frames <- frame:
		case <-ctx.Done():
			return
		}
	}
	p.readErr = scanner.Err()
}
func (p *rpcProcess) write(value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return json.NewEncoder(p.input).Encode(value)
}
func (p *rpcProcess) call(ctx context.Context, id int, method string, params any) (json.RawMessage, error) {
	if err := p.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			if method == "session/prompt" && p.session != "" {
				p.cancelSession()
			}
			return nil, ctx.Err()
		case frame, ok := <-p.frames:
			if !ok {
				if p.readErr != nil {
					return nil, p.readErr
				}
				return nil, errors.New("agent process exited before replying")
			}
			if frame.Method != "" {
				if err := p.receive(ctx, frame); err != nil {
					return nil, err
				}
				continue
			}
			if string(frame.ID) != fmt.Sprint(id) {
				continue
			}
			if frame.Error != nil {
				return nil, fmt.Errorf("ACP %s: %s", method, frame.Error.Message)
			}
			return frame.Result, nil
		}
	}
}

func (p *rpcProcess) cancelSession() {
	p.cancelOnce.Do(func() {
		session := p.getSession()
		if session == "" {
			return
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = p.write(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": session}})
		}()
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-done:
			<-timer.C
		case <-timer.C:
			// Closing the pipe releases a blocked write before we join its goroutine.
			_ = p.input.Close()
			<-done
		}
	})
}

func (p *rpcProcess) setSession(session string) {
	p.sessionMu.Lock()
	p.session = session
	p.sessionMu.Unlock()
}
func (p *rpcProcess) getSession() string {
	p.sessionMu.RLock()
	defer p.sessionMu.RUnlock()
	return p.session
}
func (p *rpcProcess) hasSession() bool { return p.getSession() != "" }
func (p *rpcProcess) receive(ctx context.Context, frame rpcFrame) error {
	var params struct {
		Session string `json:"sessionId"`
		Update  struct {
			Type    string `json:"sessionUpdate"`
			Title   string `json:"title"`
			Status  string `json:"status"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
		Tool struct {
			Title string          `json:"title"`
			Input json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []PermissionOption `json:"options"`
	}
	if json.Unmarshal(frame.Params, &params) != nil {
		return errors.New("invalid ACP parameters")
	}
	if params.Session != "" && params.Session != p.session {
		return errors.New("ACP update belongs to another session")
	}
	if frame.Method == "session/update" {
		if params.Update.Type == "agent_message_chunk" && params.Update.Content.Type == "text" {
			if p.text.Len()+len(params.Update.Content.Text) > 64000 {
				return errors.New("agent reply exceeds 64 KiB")
			}
			p.text.WriteString(params.Update.Content.Text)
		}
		if (params.Update.Type == "tool_call" || params.Update.Type == "tool_call_update") && p.opts.Progress != nil {
			p.opts.Progress(strings.TrimSpace(params.Update.Title + " " + params.Update.Status))
		}
		return nil
	}
	if len(frame.ID) == 0 {
		return nil
	}
	if frame.Method != "session/request_permission" {
		return p.write(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": rpcError{Code: -32601, Message: "Client capability unavailable"}})
	}
	outcome := map[string]string{"outcome": "cancelled"}
	if p.opts.Permission != nil {
		if len(params.Tool.Input) > 16000 {
			return errors.New("permission request is too large to review")
		}
		choice, err := p.opts.Permission(ctx, Permission{Title: params.Tool.Title, Details: string(params.Tool.Input), Options: params.Options})
		if err != nil {
			return err
		}
		for _, option := range params.Options {
			if option.ID == choice {
				outcome = map[string]string{"outcome": "selected", "optionId": choice}
				break
			}
		}
	}
	return p.write(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"outcome": outcome}})
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("agent output exceeds 64 KiB")
	}
	return b.Buffer.Write(p)
}
func runCommand(ctx context.Context, opts ProcessOptions, prompt string) (string, error) {
	command := exec.CommandContext(ctx, opts.Command, opts.Args...)
	configureProcessGroup(command)
	command.Cancel = func() error {
		terminateProcess(command)
		return nil
	}
	command.Dir = opts.CWD
	command.Env = opts.Env
	command.WaitDelay = 2 * time.Second
	command.Stdin = strings.NewReader(prompt)
	output := &boundedOutput{limit: 64000}
	command.Stdout = output
	command.Stderr = io.Discard
	defer terminateProcess(command)
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("agent command failed: %w", err)
	}
	return strings.TrimSpace(output.String()), nil
}
