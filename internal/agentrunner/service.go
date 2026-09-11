package agentrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/google/uuid"
)

type ServiceOptions struct {
	RelayURL, Secret, StatePath string
	Rooms                       []string
	Process                     ProcessOptions
	Timeout                     time.Duration
	Log                         func(error)
}
type service struct {
	options   ServiceOptions
	relay     RelayClient
	queue     *Runner
	ctx       context.Context
	mu        sync.Mutex
	decisions map[string]chan event.Event
}

func Serve(ctx context.Context, opts ServiceOptions) error {
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Minute
	}
	pubkey, err := event.PublicKey(opts.Secret)
	if err != nil {
		return err
	}
	s := &service{options: opts, ctx: ctx, decisions: map[string]chan event.Event{}, relay: RelayClient{URL: opts.RelayURL, Secret: opts.Secret, PubKey: pubkey, Rooms: opts.Rooms, OnError: opts.Log}}
	queue, err := New(Options{AllowedRooms: opts.Rooms, StatePath: opts.StatePath, Handle: s.handle, OnError: opts.Log})
	if err != nil {
		return err
	}
	s.queue = queue
	s.relay.OnEvent = s.receive
	// Start queues only after a connection is established, so journal recovery
	// can publish status before it invokes the agent process.
	s.relay.OnReady = func() { queue.Start(ctx) }
	defer queue.Stop()
	err = s.relay.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
func (s *service) receive(e event.Event) {
	s.mu.Lock()
	for _, id := range event.TagValues(e, "e") {
		if wait := s.decisions[id]; wait != nil {
			select {
			case wait <- e:
			default:
			}
		}
	}
	s.mu.Unlock()
	m, ok := s.relay.Mention(e)
	if !ok {
		return
	}
	if isCancelCommand(m.Prompt) {
		s.queue.CancelFrom(m.Room, m.Author)
		return
	}
	if err := s.queue.Enqueue(m); err != nil && !errors.Is(err, ErrDuplicate) && s.options.Log != nil {
		s.options.Log(err)
	}
}
func (s *service) handle(parent context.Context, batch []Mention) error {
	ctx, cancel := context.WithTimeout(parent, s.options.Timeout)
	defer cancel()
	first := batch[0]
	tags := [][]string{{"h", first.Room}, {"e", first.Root}, {"p", s.relay.PubKey}, {"output", "text/plain"}, {"subject", shortText(first.Prompt, 100)}, {"expiration", strconv.FormatInt(time.Now().Add(s.options.Timeout).Unix(), 10)}}
	var prompt strings.Builder
	prompt.WriteString("These are messages addressed to you in a Nostr chat. Reply to the users' request. Room: " + first.Room + "\n\n")
	for _, m := range batch {
		tags = append(tags, []string{"i", m.EventID, "event"})
		fmt.Fprintf(&prompt, "Author: %s\nMessage: %s\n%s\n\n", m.Author, m.EventID, m.Prompt)
	}
	request, err := s.relay.Publish(ctx, event.Event{Kind: 5000, CreatedAt: first.CreatedAt, Tags: tags, Content: shortText(first.Prompt, 1000)})
	if err != nil {
		return err
	}
	feedback := func(status, info string) error {
		_, err := s.relay.Publish(ctx, event.Event{Kind: 7000, Tags: [][]string{{"h", first.Room}, {"e", request.ID}, {"p", request.PubKey}, {"status", status, shortText(info, 500)}}, Content: shortText(info, 4000)})
		return err
	}
	if err := feedback("processing", "Working on your request"); err != nil {
		return err
	}
	process := s.options.Process
	var last time.Time
	process.Progress = func(info string) {
		if time.Since(last) < 2*time.Second {
			return
		}
		last = time.Now()
		if err := feedback("processing", info); err != nil {
			cancel()
		}
	}
	process.Permission = func(ctx context.Context, p Permission) (string, error) { return s.permission(ctx, batch, p) }
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() { defer close(heartbeatDone); s.heartbeat(heartbeatCtx, first.Room) }()
	output, runErr := RunProcess(ctx, process, prompt.String())
	stopHeartbeat()
	<-heartbeatDone
	finishCtx, finish := context.WithTimeout(s.ctx, 10*time.Second)
	defer finish()
	finalStatus := "idle"
	if runErr != nil {
		finalStatus = "offline"
		_ = s.publishPresence(finishCtx, first.Room, finalStatus)
		message := "Agent stopped: " + runErr.Error()
		if errors.Is(runErr, context.Canceled) {
			message = "Canceled"
		}
		_, publishErr := s.relay.Publish(finishCtx, event.Event{Kind: 7000, Tags: [][]string{{"h", first.Room}, {"e", request.ID}, {"p", request.PubKey}, {"status", "error"}}, Content: shortText(message, 1000)})
		if publishErr != nil {
			return errors.Join(runErr, publishErr)
		}
		return runErr
	}
	_ = s.publishPresence(finishCtx, first.Room, finalStatus)
	if output == "" {
		output = "Task completed."
	}
	raw, _ := json.Marshal(request)
	_, err = s.relay.Publish(ctx, event.Event{Kind: 6000, Tags: [][]string{{"h", first.Room}, {"e", request.ID}, {"p", request.PubKey}, {"request", string(raw)}}, Content: output})
	if err != nil {
		return err
	}
	replyTags := [][]string{{"h", first.Room}, {"e", first.Root, "", "root"}}
	for _, m := range batch {
		if !slices.ContainsFunc(replyTags, func(tag []string) bool { return len(tag) > 1 && tag[0] == "p" && tag[1] == m.Author }) {
			replyTags = append(replyTags, []string{"p", m.Author})
		}
	}
	_, err = s.relay.Publish(ctx, event.Event{Kind: 9, Tags: replyTags, Content: output})
	return err
}

func isCancelCommand(prompt string) bool {
	fields := strings.Fields(prompt)
	return len(fields) > 0 && (fields[0] == "/cancel" || fields[0] == "/stop")
}

func (s *service) publishPresence(ctx context.Context, room, status string) error {
	_, err := s.relay.Publish(ctx, event.Event{Kind: 20001, Tags: [][]string{{"h", room}}, Content: status})
	return err
}
func (s *service) heartbeat(ctx context.Context, room string) {
	timer := time.NewTicker(30 * time.Second)
	defer timer.Stop()
	for {
		_, err := s.relay.Publish(ctx, event.Event{Kind: 20001, Tags: [][]string{{"h", room}}, Content: "working"})
		if err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
func (s *service) permission(ctx context.Context, batch []Mention, p Permission) (string, error) {
	allow, deny := "", ""
	for _, option := range p.Options {
		if option.Kind == "allow_once" {
			allow = option.ID
		}
		if option.Kind == "reject_once" {
			deny = option.ID
		}
	}
	if allow == "" {
		return deny, nil
	}
	first := batch[0]
	expires := time.Now().Add(10 * time.Minute).Unix()
	if deadline, ok := ctx.Deadline(); ok && deadline.Unix() < expires {
		expires = deadline.Unix()
	}
	request := event.Event{Kind: 9, CreatedAt: time.Now().Unix(), Tags: [][]string{{"h", first.Room}, {"e", first.Root, "", "root"}, {"request", "approve"}, {"d", uuid.NewString()}, {"expiration", strconv.FormatInt(expires, 10)}, {"subject", shortText(p.Title, 150)}, {"p", first.Author}}, Content: "Allow this tool action once?\n\n" + shortText(p.Title, 4000) + "\n\n" + p.Details}
	if err := event.Sign(&request, s.options.Secret); err != nil {
		return "", err
	}
	wait := make(chan event.Event, 1)
	s.mu.Lock()
	s.decisions[request.ID] = wait
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.decisions, request.ID); s.mu.Unlock() }()
	if _, err := s.relay.Publish(ctx, request); err != nil {
		return "", err
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case answer := <-wait:
			if answer.PubKey != first.Author || answer.Kind != 7 {
				continue
			}
			switch strings.TrimSpace(answer.Content) {
			case "+", "":
				return allow, nil
			case "-":
				return deny, nil
			}
		}
	}
}
func shortText(value string, size int) string {
	runes := []rune(value)
	if len(runes) > size {
		return string(runes[:size]) + "…"
	}
	return value
}
