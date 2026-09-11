// Package agentrunner runs an optional agent process outside the relay daemon.
package agentrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	ErrDuplicate        = errors.New("agent runner: duplicate event")
	ErrUnauthorizedRoom = errors.New("agent runner: unauthorized room")
	ErrQueueFull        = errors.New("agent runner: room queue full")
	ErrStopped          = errors.New("agent runner: stopped")
)

type Mention struct {
	Room      string `json:"room"`
	EventID   string `json:"event_id"`
	Author    string `json:"author"`
	Prompt    string `json:"prompt"`
	Root      string `json:"root"`
	Kind      int    `json:"kind"`
	CreatedAt int64  `json:"created_at"`
}
type Handler func(context.Context, []Mention) error
type Options struct {
	AllowedRooms []string
	QueueLimit   int
	StatePath    string
	Handle       Handler
	OnError      func(error)
}
type roomQueue struct {
	pending, active []Mention
	cancel          context.CancelFunc
	wake            chan struct{}
}
type journal struct {
	Pending map[string][]Mention `json:"pending"`
	Seen    []string             `json:"seen"`
}
type Runner struct {
	mu      sync.Mutex
	rooms   map[string]*roomQueue
	seen    map[string]bool
	order   []string
	opts    Options
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool
}

func New(opts Options) (*Runner, error) {
	if opts.Handle == nil || len(opts.AllowedRooms) == 0 || len(opts.AllowedRooms) > 32 {
		return nil, errors.New("agent runner: a handler and 1 to 32 rooms are required")
	}
	if opts.QueueLimit <= 0 {
		opts.QueueLimit = 32
	}
	if opts.QueueLimit > 256 {
		return nil, errors.New("agent runner: queue limit cannot exceed 256")
	}
	r := &Runner{opts: opts, rooms: map[string]*roomQueue{}, seen: map[string]bool{}}
	for _, room := range opts.AllowedRooms {
		if room == "" {
			return nil, ErrUnauthorizedRoom
		}
		r.rooms[room] = &roomQueue{wake: make(chan struct{}, 1)}
	}
	if err := r.restore(); err != nil {
		return nil, err
	}
	return r, nil
}
func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx != nil || r.stopped {
		return
	}
	r.ctx, r.cancel = context.WithCancel(ctx)
	for room, q := range r.rooms {
		r.wg.Add(1)
		go r.work(room, q)
	}
}
func (r *Runner) Enqueue(m Mention) error {
	if m.EventID == "" || m.Author == "" || len(m.Prompt) > 64*1024 {
		return errors.New("agent runner: invalid mention")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return ErrStopped
	}
	q, ok := r.rooms[m.Room]
	if !ok {
		return ErrUnauthorizedRoom
	}
	if r.seen[m.EventID] {
		return ErrDuplicate
	}
	if len(q.pending) >= r.opts.QueueLimit {
		return ErrQueueFull
	}
	q.pending = append(q.pending, m)
	r.seen[m.EventID] = true
	r.order = append(r.order, m.EventID)
	if err := r.save(); err != nil {
		q.pending = q.pending[:len(q.pending)-1]
		delete(r.seen, m.EventID)
		r.order = r.order[:len(r.order)-1]
		return err
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}
func (r *Runner) CancelFrom(room, author string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.rooms[room]
	if q == nil {
		return false
	}
	allowed := false
	for _, m := range append(append([]Mention{}, q.active...), q.pending...) {
		if author == "" || m.Author == author {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	filtered := q.pending[:0]
	for _, m := range q.pending {
		if author == "" || m.Author == author {
			continue
		}
		filtered = append(filtered, m)
	}
	q.pending = filtered
	if q.cancel != nil && anyAuthor(q.active, author) {
		q.cancel()
	}
	if err := r.save(); err != nil {
		r.report(err)
	}
	return true
}
func (r *Runner) Cancel(room string) error { r.CancelFrom(room, ""); return nil }
func (r *Runner) Stop() {
	r.mu.Lock()
	r.stopped = true
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}
func (r *Runner) work(room string, q *roomQueue) {
	defer r.wg.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		default:
		}
		r.mu.Lock()
		if len(q.pending) == 0 {
			r.mu.Unlock()
			select {
			case <-r.ctx.Done():
				return
			case <-q.wake:
			}
			continue
		}
		batch := takeBatch(q.pending)
		q.pending = q.pending[len(batch):]
		q.active = batch
		ctx, cancel := context.WithCancel(r.ctx)
		q.cancel = cancel
		if err := r.save(); err != nil {
			r.report(err)
			cancel()
			q.pending = append(batch, q.pending...)
			q.active = nil
			q.cancel = nil
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		err := r.opts.Handle(ctx, batch)
		cancel()
		r.mu.Lock()
		if r.ctx.Err() != nil {
			q.pending = append(batch, q.pending...)
		}
		q.active = nil
		q.cancel = nil
		if saveErr := r.save(); saveErr != nil {
			r.report(saveErr)
		}
		r.mu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			r.report(fmt.Errorf("agent room %s: %w", room, err))
		}
	}
}

func takeBatch(pending []Mention) []Mention {
	if len(pending) == 0 {
		return nil
	}
	root := pending[0].Root
	n := 1
	for n < len(pending) && pending[n].Root == root {
		n++
	}
	return append([]Mention(nil), pending[:n]...)
}

func anyAuthor(events []Mention, author string) bool {
	if author == "" {
		return len(events) > 0
	}
	for _, event := range events {
		if event.Author == author {
			return true
		}
	}
	return false
}
func (r *Runner) report(err error) {
	if r.opts.OnError != nil {
		r.opts.OnError(err)
	}
}
func (r *Runner) restore() error {
	if r.opts.StatePath == "" {
		return nil
	}
	raw, err := os.ReadFile(r.opts.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read agent state: %w", err)
	}
	if len(raw) > 32<<20 {
		return errors.New("agent state exceeds 32 MiB")
	}
	var state journal
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("decode agent state: %w", err)
	}
	for _, id := range state.Seen {
		if !r.seen[id] {
			r.seen[id] = true
			r.order = append(r.order, id)
		}
	}
	for room, queue := range state.Pending {
		q := r.rooms[room]
		if q == nil {
			return errors.New("agent state contains an unconfigured room")
		}
		if len(queue) > r.opts.QueueLimit*2 {
			return ErrQueueFull
		}
		q.pending = queue
	}
	return nil
}
func (r *Runner) save() error {
	if r.opts.StatePath == "" {
		return nil
	}
	// Keep a bounded replay history while retaining every queued event.
	active := map[string]bool{}
	pending := map[string][]Mention{}
	for room, q := range r.rooms {
		pending[room] = append(append([]Mention{}, q.active...), q.pending...)
		for _, m := range pending[room] {
			active[m.EventID] = true
		}
	}
	if len(r.order) > 10000 {
		keep := make(map[string]bool, len(active)+10000)
		for id := range active {
			keep[id] = true
		}
		remaining := 10000 - len(active)
		if remaining < 0 {
			remaining = 0
		}
		for i := len(r.order) - 1; i >= 0 && remaining > 0; i-- {
			if !keep[r.order[i]] {
				keep[r.order[i]] = true
				remaining--
			}
		}
		kept := make([]string, 0, len(keep))
		for _, id := range r.order {
			if keep[id] {
				kept = append(kept, id)
			}
		}
		for id := range r.seen {
			if !keep[id] {
				delete(r.seen, id)
			}
		}
		r.order = kept
	}
	raw, err := json.Marshal(journal{Pending: pending, Seen: r.order})
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.opts.StatePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".tiny-agent-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, r.opts.StatePath)
}

// Drain waits until queued work has finished, or the caller cancels.
func (r *Runner) Drain(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		r.mu.Lock()
		empty := true
		for _, q := range r.rooms {
			empty = empty && len(q.pending) == 0 && len(q.active) == 0
		}
		r.mu.Unlock()
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
