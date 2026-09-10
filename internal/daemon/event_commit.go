package daemon

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// eventCommitter prepares the projections common to client publication and
// replication. Admission and client-only protocol actions remain with their
// entry points; every projection here participates in the event transaction.
type eventCommitter struct {
	policy    policy.Policy
	community *community.Service
	sites     *sites.Service
	git       *gitrelay.GitRelay
}

type eventCommitPlan struct {
	options     storage.SaveOptions
	repository  gitrelay.Repository
	gitMetadata bool
	followups   *storage.Intent
}

func (c eventCommitter) prepare(ctx context.Context, e event.Event, origin replication.Origin, now int64) (eventCommitPlan, error) {
	plan := eventCommitPlan{options: storage.SaveOptions{Now: now}}
	if c.sites != nil {
		plan.options = c.sites.SaveOptions(e, now)
	}
	plan.options.SearchMode = c.policy.Features.Search
	if e.Kind == 30023 {
		plan.options.Intents = append(plan.options.Intents, storage.Intent{Kind: "view-publish", EventID: e.ID, Target: "articles", Payload: "{}"})
	}
	if e.Kind == event.KIND_AGENT_GRANT || e.Kind == event.KIND_DELETION {
		plan.options.AddBeforeCommit(func(ctx context.Context, tx *sql.Tx) error {
			return c.community.ApplyAgentEventTx(ctx, tx, e, now)
		})
	}
	if c.policy.Features.Grasp && (e.Kind == event.KIND_REPO || e.Kind == event.KIND_REPO_STATE) {
		if err := c.prepareRepository(ctx, e, origin, &plan); err != nil {
			return eventCommitPlan{}, err
		}
	}
	return plan, nil
}

func (c eventCommitter) prepareRepository(ctx context.Context, e event.Event, origin replication.Origin, plan *eventCommitPlan) error {
	validate := c.git.ValidateImported
	if origin == replication.OriginClient {
		validate = c.git.Validate
	}
	repo, err := validate(ctx, e)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(repo)
	if err != nil {
		return err
	}
	plan.repository, plan.gitMetadata = repo, true
	plan.options.Intents = append(plan.options.Intents, storage.Intent{Kind: "git-metadata", EventID: e.ID, Target: repo.Owner + ":" + repo.Identifier, Payload: string(raw)})
	if e.Kind == event.KIND_REPO_STATE {
		plan.options.AddBeforeCommit(func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO pending_events(id,reason) VALUES(?,'git objects')", e.ID)
			return err
		})
	}
	return nil
}

func (t *Tenant) validateStoredEvent(ctx context.Context, e event.Event) error {
	if err := t.validatePrivateRepositoryPlacement(ctx, e); err != nil {
		return err
	}
	if err := validateReviewAnchor(e); err != nil {
		return err
	}
	if t.sites != nil {
		return sites.ValidateManifest(e)
	}
	return nil
}

func (t *Tenant) prepareStoredEvent(ctx context.Context, e event.Event, origin replication.Origin, now int64) (eventCommitPlan, error) {
	committer := eventCommitter{policy: t.Policy(), community: t.community, sites: t.sites, git: t.git}
	plan, err := committer.prepare(ctx, e, origin, now)
	if err != nil {
		return plan, err
	}
	var push *replicationPushFollowupPayload
	if origin == replication.OriginClient && t.Policy().Features.Push {
		payload, _ := t.prepareReplicationPushFollowup(ctx, e, now)
		if len(payload.RegistrationIDs) != 0 || payload.Discover {
			push = &payload
		}
	}
	plan.followups = t.followups.prepareWithPush(ctx, e, now, false, push)
	if plan.followups != nil {
		plan.options.Intents = append(plan.options.Intents, *plan.followups)
	}
	return plan, nil
}

func (t *Tenant) afterStoredEvent(ctx context.Context, e event.Event, followups *storage.Intent) {
	t.notifyDevices(ctx, e)
	t.followups.try(ctx, followups)
	if event.IsEphemeral(e.Kind) {
		t.notifyCallbacks(ctx, e)
	}
	t.customViews.cascadeViewDeletion(ctx, e)
	t.notifyWikiMerge(ctx, e)
	t.notifyWikiProposal(ctx, e)
}
