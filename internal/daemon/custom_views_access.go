package daemon

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/views"
)

func (t *customViewService) artifactEventVisible(ctx context.Context, e event.Event, s relay.Session) bool {
	if t.records == nil || e.PubKey != t.records.PublicKey() {
		return true
	}
	d := event.Tag(e, "d")
	if !strings.HasPrefix(d, "bind.ws/view/") {
		return true
	}
	var view, hash string
	if err := t.store.DB().QueryRowContext(ctx, `SELECT view,hash FROM custom_view_artifacts WHERE event_id=?`, e.ID).Scan(&view, &hash); err != nil {
		return false
	}
	if d != "bind.ws/view/"+view+"/"+hash {
		return false
	}
	refs := map[string]bool{}
	for _, tag := range e.Tags {
		if len(tag) == 2 && (tag[0] == "e" || tag[0] == "a") {
			refs[tag[1]] = true
		}
	}
	if len(refs) == 0 {
		return false
	}
	sources, ok := t.artifactSourceRows(ctx, view, hash)
	if !ok {
		return false
	}
	for _, source := range sources {
		if !refs[source] || !t.sourceVisible(ctx, source, s, hash) {
			return false
		}
		delete(refs, source)
	}
	return len(sources) > 0 && len(refs) == 0
}

func (t *customViewService) artifactHTTPSourceVisible(ctx context.Context, view, hash string, s relay.Session) bool {
	sources, ok := t.artifactSourceRows(ctx, view, hash)
	if !ok {
		return false
	}
	for _, source := range sources {
		if t.sourceVisible(ctx, source, s, hash) {
			return true
		}
	}
	return false
}

func (t *customViewService) artifactSourceRows(ctx context.Context, view, hash string) ([]string, bool) {
	rows, err := t.store.DB().QueryContext(ctx, `SELECT source FROM custom_view_sources WHERE view=? AND hash=?`, view, hash)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var sources []string
	for rows.Next() {
		var source string
		if rows.Scan(&source) != nil {
			_ = rows.Close()
			return nil, false
		}
		sources = append(sources, source)
	}
	if rows.Err() != nil {
		return nil, false
	}
	return sources, true
}

func (t *customViewService) sourceVisible(ctx context.Context, source string, s relay.Session, artifactHash string) bool {
	if t.gate == nil {
		return false
	}
	var raw string
	if tag := viewSourceTag(source); tag[0] == "a" {
		parts := strings.SplitN(source, ":", 3)
		if len(parts) != 3 {
			return false
		}
		if err := t.store.DB().QueryRowContext(ctx, `SELECT raw FROM events WHERE kind=? AND pubkey=? AND d=? AND (expires=0 OR expires>?) ORDER BY created_at DESC,id ASC LIMIT 1`, event.KIND_REPO_STATE, parts[1], parts[2], time.Now().Unix()).Scan(&raw); err != nil {
			return false
		}
	} else if err := t.store.DB().QueryRowContext(ctx, `SELECT raw FROM events WHERE id=? AND (expires=0 OR expires>?)`, source, time.Now().Unix()).Scan(&raw); err != nil {
		return false
	}
	var sourceEvent event.Event
	if err := json.Unmarshal([]byte(raw), &sourceEvent); err != nil || !t.gate.CanSee(ctx, sourceEvent, s, nil) {
		return false
	}
	if strings.HasPrefix(source, strconv.Itoa(event.KIND_REPO_STATE)+":") && artifactHash != "" {
		blocks, err := t.sourceBlocks(ctx, sourceEvent)
		if err != nil {
			return false
		}
		for _, block := range blocks {
			if views.Hash(block.Lang, block.Source) == artifactHash {
				return true
			}
		}
		return false
	}
	return true
}
