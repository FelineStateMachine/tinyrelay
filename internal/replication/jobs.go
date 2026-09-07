package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

type JobKind string

const (
	JobPull   JobKind = "pull"
	JobPush   JobKind = "push"
	JobImport JobKind = "import"
	JobMirror JobKind = "mirror"
	JobDump   JobKind = "dump"
	JobBackup JobKind = "backup"
)

type JobSpec struct {
	ID             string   `json:"id"`
	Kind           JobKind  `json:"kind"`
	Relays         []string `json:"relays,omitempty"`
	Filter         string   `json:"filter,omitempty"`
	Every          int      `json:"every,omitempty"`
	DiscoverPubKey string   `json:"discoverPubKey,omitempty"`
}

type JobRunner interface {
	Run(ctx context.Context, spec JobSpec) error
}

type JobHandler struct {
	runner JobRunner
}

func NewJobHandler(runner JobRunner) *JobHandler { return &JobHandler{runner: runner} }

func (h *JobHandler) Handle(ctx context.Context, intent work.Intent) error {
	if h == nil || h.runner == nil {
		return errors.New("replication: no job runner configured")
	}
	var spec JobSpec
	if err := json.Unmarshal([]byte(intent.Payload), &spec); err != nil {
		return fmt.Errorf("replication: decode job: %w", err)
	}
	if err := ValidateJob(spec); err != nil {
		return err
	}
	return h.runner.Run(ctx, spec)
}

func ValidateJob(spec JobSpec) error {
	switch spec.Kind {
	case JobPull, JobPush, JobImport, JobMirror, JobDump, JobBackup:
	default:
		return fmt.Errorf("replication: unsupported job kind %q", spec.Kind)
	}
	if spec.ID == "" {
		return errors.New("replication: job id is required")
	}
	if spec.Every < 0 {
		return errors.New("replication: job interval cannot be negative")
	}
	if spec.Filter != "" {
		if _, err := event.ParseFilter([]byte(spec.Filter)); err != nil {
			return fmt.Errorf("replication: invalid job filter: %w", err)
		}
	}
	if spec.Kind == JobPull || spec.Kind == JobPush {
		if len(spec.Relays) == 0 && !(spec.Kind == JobPull && spec.DiscoverPubKey != "") {
			return errors.New("replication: relay job needs a target")
		}
		for _, relay := range spec.Relays {
			if !safeRelayURL(relay) {
				return fmt.Errorf("replication: invalid relay target %q", relay)
			}
		}
	}
	return nil
}

func EnqueueJob(ctx context.Context, queue *work.Queue, spec JobSpec, at time.Time) (string, error) {
	if err := ValidateJob(spec); err != nil {
		return "", err
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("replication: encode job: %w", err)
	}
	id := fmt.Sprintf("%s/%d", spec.ID, at.Unix())
	// The schedule timestamp makes each recurring execution a distinct
	// durable intent. The work table intentionally deduplicates identical
	// (kind,event,target) tuples, so a stable target would drop later runs.
	return queue.Enqueue(ctx, work.Intent{ID: id, Kind: "job", EventID: spec.ID, Target: string(spec.Kind) + ":" + id, Payload: string(payload), NextAt: at})
}

func CancelJob(ctx context.Context, queue *work.Queue, id string, now time.Time) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("replication: job id is required")
	}
	return queue.Cancel(ctx, id, now)
}

// RescheduleJob makes a recurring job eligible again after a successful run.
// It is separate from completion so the completed intent remains an audit
// record and a fresh run receives an independent claim token.
func RescheduleJob(ctx context.Context, queue *work.Queue, spec JobSpec, at time.Time) (string, error) {
	if spec.Every <= 0 {
		return "", errors.New("replication: only recurring jobs can be rescheduled")
	}
	return EnqueueJob(ctx, queue, spec, at.Add(time.Duration(spec.Every)*time.Hour))
}

func IntentForJob(spec JobSpec, at time.Time) (storage.Intent, error) {
	if err := ValidateJob(spec); err != nil {
		return storage.Intent{}, err
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		return storage.Intent{}, fmt.Errorf("replication: encode job intent: %w", err)
	}
	return storage.Intent{Kind: "job", EventID: spec.ID, Target: string(spec.Kind), Payload: string(payload)}, nil
}
