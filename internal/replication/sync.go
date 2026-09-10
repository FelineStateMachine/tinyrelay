package replication

import (
	"context"
	"fmt"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// PullTransport reads events from target. The transport owns network clients
// and their lifecycle; PullOnce owns validation and storage.
type PullTransport interface {
	Query(ctx context.Context, target string, filter event.Filter) ([]event.Event, error)
}

// PushTransport sends one event to target. PushAfter advances its cursor only
// after Send returns, including a relay duplicate acknowledgment.
type PushTransport interface {
	Send(ctx context.Context, target string, e event.Event) (DeliveryResult, error)
}

// SyncStats summarizes one pull or push pass.
type SyncStats struct {
	Stored     int
	Duplicates int
	Rejected   int
	Sent       int
	Refused    int
}

func PullOnce(ctx context.Context, store *storage.Store, transport PullTransport, target string, filter event.Filter, now int64) (SyncStats, error) {
	return PullOnceWithIngest(ctx, store, transport, target, filter, now, nil)
}

func PullOnceWithIngest(ctx context.Context, store *storage.Store, transport PullTransport, target string, filter event.Filter, now int64, ingest func(context.Context, event.Event, Origin) error) (SyncStats, error) {
	if !safeRelayURL(target) {
		return SyncStats{}, fmt.Errorf("replication: invalid pull target %q", target)
	}
	items, err := transport.Query(ctx, target, filter)
	if err != nil {
		return SyncStats{}, fmt.Errorf("replication: query %s: %w", target, err)
	}
	var stats SyncStats
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if err := event.Validate(item); err != nil {
			stats.Rejected++
			continue
		}
		var saveErr error
		if ingest != nil {
			saveErr = ingest(ctx, item, OriginImport)
		} else {
			_, saveErr = store.Save(ctx, item, storage.SaveOptions{Now: now})
		}
		switch {
		case saveErr == nil:
			stats.Stored++
		case saveErr == storage.ErrDuplicate || saveErr == storage.ErrReplaced:
			stats.Duplicates++
		default:
			stats.Rejected++
		}
	}
	return stats, nil
}

func PushAfter(ctx context.Context, store *storage.Store, transport PushTransport, target string, cursor int64, filter event.Filter, now int64) (SyncStats, int64, error) {
	if !safeRelayURL(target) {
		return SyncStats{}, cursor, fmt.Errorf("replication: invalid push target %q", target)
	}
	rows, err := store.After(ctx, cursor, filter, storage.QueryOptions{Now: now, Access: storage.Access{All: true}, Limit: 0})
	if err != nil {
		return SyncStats{}, cursor, fmt.Errorf("replication: read push cursor: %w", err)
	}
	var stats SyncStats
	last := cursor
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return stats, last, err
		}
		result, sendErr := transport.Send(ctx, target, row.Event)
		if sendErr != nil {
			return stats, last, fmt.Errorf("replication: push event: %w", sendErr)
		}
		if result.Accepted || isDuplicate(result.Message) {
			stats.Sent++
		} else {
			stats.Refused++
		}
		last = row.Sequence
	}
	return stats, last, nil
}

func NextRun(spec JobSpec, finished time.Time) time.Time {
	if spec.Every <= 0 {
		return time.Time{}
	}
	return finished.Add(time.Duration(spec.Every) * time.Hour)
}
