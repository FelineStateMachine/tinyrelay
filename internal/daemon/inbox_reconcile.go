package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
)

func (t *Tenant) reconcileAutomaticInbox(ctx context.Context, previous, current policy.Policy) error {
	if t.replication == nil {
		return nil
	}
	jobs, err := t.replication.ListJobs(ctx)
	if err != nil {
		return fmt.Errorf("daemon: list inbox jobs: %w", err)
	}
	jobID := "source-" + t.meta.ID
	var automatic *replication.JobSpec
	var explicit bool
	for _, job := range jobs {
		if job.ID != jobID {
			continue
		}
		if job.DiscoverPubKey != "" {
			copy := job
			automatic = &copy
		} else {
			explicit = true
		}
	}
	if !current.Inbox.Targeted {
		if automatic == nil {
			return nil
		}
		return t.replication.RemoveJob(ctx, jobID)
	}
	if automatic == nil && explicit {
		return nil
	}
	owner := current.Owner
	kinds, err := t.inboxAllowedKinds(ctx, current)
	if err != nil {
		return err
	}
	filter := map[string]any{"#p": []string{owner}}
	if len(kinds) > 0 {
		filter["kinds"] = kinds
	}
	raw, err := json.Marshal(filter)
	if err != nil {
		return fmt.Errorf("daemon: encode inbox filter: %w", err)
	}
	desired := replication.JobSpec{ID: jobID, Kind: replication.JobPull, Filter: string(raw), Every: 1, DiscoverPubKey: owner}
	if automatic != nil && sameInboxJob(*automatic, desired) {
		return nil
	}
	if automatic != nil {
		if err := t.replication.RemoveJob(ctx, jobID); err != nil {
			return fmt.Errorf("daemon: replace inbox job: %w", err)
		}
	}
	if err := t.replication.AddJob(ctx, desired); err != nil {
		return fmt.Errorf("daemon: save inbox job: %w", err)
	}
	return nil
}

func (t *Tenant) inboxAllowedKinds(ctx context.Context, p policy.Policy) ([]int, error) {
	if len(p.AllowedKinds) > 0 {
		return append([]int(nil), p.AllowedKinds...), nil
	}
	rows, err := t.store.DB().QueryContext(ctx, `SELECT kind FROM community_kind_rules WHERE rule='allow' ORDER BY kind`)
	if err != nil {
		return nil, fmt.Errorf("daemon: read inbox kind rules: %w", err)
	}
	defer rows.Close()
	var kinds []int
	for rows.Next() {
		var kind int
		if err := rows.Scan(&kind); err != nil {
			return nil, err
		}
		kinds = append(kinds, kind)
	}
	return kinds, rows.Err()
}

func inboxJobOwner(job replication.JobSpec) string {
	filter, err := event.ParseFilter([]byte(job.Filter))
	if err != nil || len(filter.Tags["p"]) == 0 {
		return ""
	}
	return filter.Tags["p"][0]
}

func sameInboxJob(a, b replication.JobSpec) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.Filter == b.Filter && a.Every == b.Every && a.DiscoverPubKey == b.DiscoverPubKey && len(a.Relays) == 0 && len(b.Relays) == 0
}
