package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

type Config struct {
	Store         *storage.Store
	Policy        Policy
	CurrentPolicy func() Policy
	Directory     RelayDirectory
	// Owner identifies the tenant owner for owner-scoped management jobs.
	// It is used by backfill when the UI leaves the relay list blank.
	Owner           func() string
	Discovery       Discovery
	Delivery        DeliveryTransport
	DeliveryAllowed func(context.Context, storage.Intent) bool
	Pull            PullTransport
	Push            PushTransport
	DataDir         string
	Ingest          func(context.Context, event.Event, Origin) error
	ExecuteExternal func(context.Context, JobSpec) error
	BackupProvider  BackupProvider
	Workers         int
}

type Service struct {
	store  *storage.Store
	queue  *work.Queue
	config Config
}

func NewService(config Config) (*Service, error) {
	if config.Store == nil {
		return nil, errors.New("replication: store is required")
	}
	if config.DataDir == "" {
		config.DataDir = filepath.Dir(config.Store.Path())
	}
	if err := createJobStateTable(config.Store); err != nil {
		return nil, err
	}
	return &Service{store: config.Store, queue: work.New(config.Store), config: config}, nil
}

func (s *Service) Store() *storage.Store { return s.store }

func (s *Service) Queue() *work.Queue { return s.queue }

func (s *Service) Prepare(e event.Event, origin Origin) []storage.Intent {
	directory := s.config.Directory
	if directory == nil {
		policy := s.config.Policy
		if s.config.CurrentPolicy != nil {
			policy = s.config.CurrentPolicy()
		}
		if !eligible(e, origin, policy) {
			return nil
		}
		payload, _ := json.Marshal(IntentPayload{Origin: origin})
		return []storage.Intent{{Kind: "delivery-discovery", EventID: e.ID, Target: e.PubKey, Payload: string(payload)}}
	}
	policy := s.config.Policy
	if s.config.CurrentPolicy != nil {
		policy = s.config.CurrentPolicy()
	}
	intents := PrepareIntents(context.Background(), e, origin, policy, directory)
	if len(intents) == 0 && eligible(e, origin, policy) {
		payload, _ := json.Marshal(IntentPayload{Origin: origin})
		return []storage.Intent{{Kind: "delivery-discovery", EventID: e.ID, Target: e.PubKey, Payload: string(payload)}}
	}
	return intents
}

func (s *Service) resolveDiscovery(ctx context.Context, e event.Event) RelayDirectory {
	if s.config.Discovery == nil {
		return nil
	}
	writes, err := s.config.Discovery.DiscoverRelays(ctx, e.PubKey)
	if err != nil {
		return nil
	}
	directory := staticDirectory{writes: writes, reads: map[string][]string{}}
	if discovery, ok := s.config.Discovery.(ReadDiscovery); ok {
		for _, recipient := range event.TagValues(e, "p") {
			reads, readErr := discovery.DiscoverReadRelays(ctx, recipient)
			if readErr == nil {
				directory.reads[recipient] = reads
			}
		}
	}
	return directory
}

type staticDirectory struct {
	writes []string
	reads  map[string][]string
}

func (d staticDirectory) WriteRelays(string) []string       { return d.writes }
func (d staticDirectory) ReadRelays(pubkey string) []string { return d.reads[pubkey] }

func (s *Service) AddJob(ctx context.Context, spec JobSpec) error {
	if _, err := EnqueueJob(ctx, s.queue, spec, time.Now()); err != nil {
		return err
	}
	return s.saveJobState(ctx, spec)
}

func (s *Service) ListJobs(ctx context.Context) ([]JobSpec, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT payload FROM replication_jobs WHERE state!='cancelled' ORDER BY created_at,id`)
	if err != nil {
		return nil, fmt.Errorf("replication: list jobs: %w", err)
	}
	defer rows.Close()
	jobs := make([]JobSpec, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var spec JobSpec
		if json.Unmarshal([]byte(raw), &spec) == nil {
			jobs = append(jobs, spec)
		}
	}
	return jobs, rows.Err()
}

func (s *Service) RemoveJob(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("replication: job id is required")
	}
	_, err := s.store.DB().ExecContext(ctx, `UPDATE replication_jobs SET state='cancelled',updated_at=? WHERE id=?`, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	_, err = s.store.DB().ExecContext(ctx, `UPDATE work_intents SET state='cancelled',updated_at=?,claim_token='',claim_until=0 WHERE event_id=? AND kind='job'`, time.Now().Unix(), id)
	return err
}

func (s *Service) RunJob(ctx context.Context, id string) error {
	if _, err := s.store.DB().ExecContext(ctx, `UPDATE replication_jobs SET state='pending',updated_at=? WHERE id=? AND state!='cancelled'`, time.Now().Unix(), id); err != nil {
		return err
	}
	if _, err := s.store.DB().ExecContext(ctx, `UPDATE work_intents SET state='pending',next_at=?,updated_at=?,claim_token='',claim_until=0 WHERE event_id=? AND kind='job' AND state!='completed' AND state!='cancelled'`, time.Now().Unix(), time.Now().Unix(), id); err != nil {
		return fmt.Errorf("replication: run job: %w", err)
	}
	if err := s.saveJobState(ctx, s.jobSpec(ctx, id)); err != nil {
		return err
	}
	return nil
}

// Handlers returns the replication-owned durable work handlers. The daemon
// composes these with tenant handlers before starting its worker pool.
func (s *Service) Handlers() map[string]work.Handler {
	handlers := map[string]work.Handler{"job": s.handleJob, "delivery-discovery": s.handleDiscovery}
	if s.config.Delivery != nil {
		delivery := NewDeliveryHandler(s.store, s.config.Delivery)
		handlers["delivery"] = func(ctx context.Context, intent work.Intent) error {
			storageIntent := storage.Intent{Kind: intent.Kind, EventID: intent.EventID, Target: intent.Target, Payload: intent.Payload}
			if s.config.DeliveryAllowed != nil && !s.config.DeliveryAllowed(ctx, storageIntent) {
				return nil
			}
			if s.config.DeliveryAllowed == nil && !s.deliveryAllowed(ctx, storageIntent) {
				return nil
			}
			return delivery(ctx, storageIntent)
		}
	}
	return handlers
}

// Run is a convenience for replication-only callers. Applications that own
// additional durable work should compose Handlers and call work.RunPool.
func (s *Service) Run(ctx context.Context) error {
	return work.RunPool(ctx, s.queue, s.Handlers(), work.PoolOptions{Workers: s.config.Workers})
}

func (s *Service) handleDiscovery(ctx context.Context, intent work.Intent) error {
	if s.config.Discovery == nil {
		return errors.New("replication: relay discovery is not configured")
	}
	result, err := s.store.Query(ctx, event.Filter{IDs: []string{intent.EventID}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		return err
	}
	if len(result.Events) == 0 {
		return nil
	}
	policy := s.config.Policy
	if s.config.CurrentPolicy != nil {
		policy = s.config.CurrentPolicy()
	}
	directory := s.resolveDiscovery(ctx, result.Events[0])
	if directory == nil {
		return errors.New("replication: no relay routing evidence yet")
	}
	intents := PrepareIntents(ctx, result.Events[0], OriginClient, policy, directory)
	if len(intents) == 0 {
		return errors.New("replication: no relay routing evidence yet")
	}
	for _, delivery := range intents {
		if _, err := s.queue.Enqueue(ctx, work.Intent{Kind: delivery.Kind, EventID: delivery.EventID, Target: delivery.Target, Payload: delivery.Payload}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) deliveryAllowed(ctx context.Context, intent storage.Intent) bool {
	policy := s.config.Policy
	if s.config.CurrentPolicy != nil {
		policy = s.config.CurrentPolicy()
	}
	result, err := s.store.Query(ctx, event.Filter{IDs: []string{intent.EventID}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err != nil || len(result.Events) == 0 {
		return false
	}
	directory := s.config.Directory
	if directory == nil && s.config.Discovery != nil {
		relays, discoverErr := s.config.Discovery.DiscoverRelays(ctx, result.Events[0].PubKey)
		if discoverErr != nil {
			return false
		}
		directory = staticDirectory{writes: relays, reads: map[string][]string{}}
	}
	if directory == nil {
		return false
	}
	return containsIntent(PrepareIntents(ctx, result.Events[0], OriginClient, policy, directory), intent.Target)
}

func containsIntent(intents []storage.Intent, target string) bool {
	for _, intent := range intents {
		if intent.Target == target {
			return true
		}
	}
	return false
}

func (s *Service) handleJob(ctx context.Context, intent work.Intent) error {
	var spec JobSpec
	if err := json.Unmarshal([]byte(intent.Payload), &spec); err != nil {
		return err
	}
	if err := ValidateJob(spec); err != nil {
		return err
	}
	started := time.Now().Unix()
	if _, err := s.store.DB().ExecContext(ctx, `UPDATE replication_jobs SET phase='running',error='',started_at=?,updated_at=? WHERE id=?`, started, started, spec.ID); err != nil {
		return fmt.Errorf("replication: mark job running: %w", err)
	}
	var runErr error
	if s.config.ExecuteExternal != nil {
		runErr = s.config.ExecuteExternal(ctx, spec)
	} else {
		switch spec.Kind {
		case JobPull:
			runErr = s.pull(ctx, spec)
		case JobPush:
			runErr = s.push(ctx, spec)
		case JobImport:
			runErr = s.importFile(ctx, spec)
		case JobDump:
			runErr = s.dumpFile(ctx, spec)
		case JobBackup:
			runErr = s.backupFile(ctx, spec)
		case JobMirror:
			runErr = s.mirror(ctx, spec)
		default:
			runErr = fmt.Errorf("replication: no local executor for %s", spec.Kind)
		}
	}
	if runErr != nil {
		if _, updateErr := s.store.DB().ExecContext(ctx, `UPDATE replication_jobs SET phase='failed',error=?,finished_at=?,updated_at=? WHERE id=?`, runErr.Error(), time.Now().Unix(), time.Now().Unix(), spec.ID); updateErr != nil {
			return errors.Join(runErr, fmt.Errorf("replication: mark job failed: %w", updateErr))
		}
		return runErr
	}
	if _, err := s.store.DB().ExecContext(ctx, `UPDATE replication_jobs SET phase='complete',error='',finished_at=?,updated_at=? WHERE id=?`, time.Now().Unix(), time.Now().Unix(), spec.ID); err != nil {
		return fmt.Errorf("replication: mark job complete: %w", err)
	}
	if spec.Every > 0 {
		_, err := EnqueueJob(ctx, s.queue, spec, NextRun(spec, time.Now()))
		return err
	}
	return nil
}

func (s *Service) pull(ctx context.Context, spec JobSpec) error {
	if s.config.Pull == nil {
		return errors.New("replication: pull transport is not configured")
	}
	filter, err := parseJobFilter(spec.Filter)
	if err != nil {
		return err
	}
	relays := append([]string(nil), spec.Relays...)
	if len(relays) == 0 && spec.DiscoverPubKey != "" {
		discovery, ok := s.config.Discovery.(ReadDiscovery)
		if !ok {
			return errors.New("replication: discovery is not configured")
		}
		relays, err = discovery.DiscoverReadRelays(ctx, spec.DiscoverPubKey)
		if err != nil {
			return fmt.Errorf("replication: discover inbox relays: %w", err)
		}
	}
	var total SyncStats
	for _, target := range relays {
		stats, err := PullOnceWithIngest(ctx, s.store, s.config.Pull, target, filter, time.Now().Unix(), s.config.Ingest)
		total.Stored += stats.Stored
		total.Duplicates += stats.Duplicates
		total.Rejected += stats.Rejected
		if err != nil {
			return err
		}
	}
	return s.recordJobStats(ctx, spec.ID, total, 0, len(relays))
}

func (s *Service) push(ctx context.Context, spec JobSpec) error {
	if s.config.Push == nil {
		return errors.New("replication: push transport is not configured")
	}
	filter, err := parseJobFilter(spec.Filter)
	if err != nil {
		return err
	}
	var total SyncStats
	var lastCursor int64
	for _, target := range spec.Relays {
		cursor := s.loadCursor(ctx, spec.ID, target)
		stats, nextCursor, err := PushAfter(ctx, s.store, s.config.Push, target, cursor, filter, time.Now().Unix())
		total.Sent += stats.Sent
		total.Refused += stats.Refused
		lastCursor = nextCursor
		if err != nil {
			return err
		}
		if err := s.saveCursor(ctx, spec.ID, target, nextCursor); err != nil {
			return err
		}
	}
	return s.recordJobStats(ctx, spec.ID, total, lastCursor, len(spec.Relays))
}

func (s *Service) importFile(ctx context.Context, spec JobSpec) error {
	if len(spec.Relays) != 1 {
		return errors.New("replication: import job needs one local file target")
	}
	path, err := safeArtifactPath(s.config.DataDir, spec.Relays[0])
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if s.config.Ingest != nil {
		return readJSONLEvents(ctx, file, func(item event.Event) error { return s.config.Ingest(ctx, item, OriginImport) })
	}
	return ImportJSONL(ctx, s.store, file, time.Now().Unix())
}

func (s *Service) dumpFile(ctx context.Context, spec JobSpec) error {
	name := spec.ID + ".jsonl"
	if len(spec.Relays) == 1 {
		name = spec.Relays[0]
	}
	path, err := safeArtifactPath(s.config.DataDir, name)
	if err != nil {
		return err
	}
	return writeArtifact(path, func(output io.Writer) error { return WriteDump(ctx, s.store, output, time.Now().Unix()) })
}

func (s *Service) backupFile(ctx context.Context, spec JobSpec) error {
	name := spec.ID + ".backup.json"
	if len(spec.Relays) == 1 {
		name = spec.Relays[0]
	}
	path, err := safeArtifactPath(s.config.DataDir, name)
	if err != nil {
		return err
	}
	if s.config.BackupProvider != nil {
		archive, err := CreateBackupWithProvider(ctx, s.store, s.config.BackupProvider, time.Now().Unix())
		if err != nil {
			return err
		}
		return writeArtifact(path, func(output io.Writer) error { return json.NewEncoder(output).Encode(archive) })
	}
	return writeArtifact(path, func(output io.Writer) error { return WriteBackup(ctx, s.store, output, time.Now().Unix()) })
}

func (s *Service) mirror(ctx context.Context, spec JobSpec) error {
	// A mirror job uses the same durable pull cursor path for relay content;
	// hosts with site/blob storage can additionally supply ExecuteExternal.
	if s.config.Pull == nil {
		return errors.New("replication: mirror transport is not configured")
	}
	return s.pull(ctx, spec)
}

func createJobStateTable(store *storage.Store) error {
	_, err := store.DB().Exec(`CREATE TABLE IF NOT EXISTS replication_jobs (id TEXT PRIMARY KEY, payload TEXT NOT NULL, state TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, phase TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '', stored INTEGER NOT NULL DEFAULT 0, sent INTEGER NOT NULL DEFAULT 0, skipped INTEGER NOT NULL DEFAULT 0, refused INTEGER NOT NULL DEFAULT 0, cursor INTEGER NOT NULL DEFAULT 0, rounds INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0); CREATE TABLE IF NOT EXISTS replication_job_cursors (job_id TEXT NOT NULL, target TEXT NOT NULL, cursor INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(job_id,target))`)
	return err
}

func (s *Service) saveJobState(ctx context.Context, spec JobSpec) error {
	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = s.store.DB().ExecContext(ctx, `INSERT INTO replication_jobs(id,payload,state,created_at,updated_at) VALUES(?,?, 'pending',?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,state='pending',updated_at=excluded.updated_at`, spec.ID, string(raw), now, now)
	return err
}

func (s *Service) jobSpec(ctx context.Context, id string) JobSpec {
	var raw string
	_ = s.store.DB().QueryRowContext(ctx, `SELECT payload FROM replication_jobs WHERE id=?`, id).Scan(&raw)
	var spec JobSpec
	_ = json.Unmarshal([]byte(raw), &spec)
	return spec
}

func (s *Service) loadCursor(ctx context.Context, id, target string) int64 {
	var cursor int64
	_ = s.store.DB().QueryRowContext(ctx, `SELECT cursor FROM replication_job_cursors WHERE job_id=? AND target=?`, id, target).Scan(&cursor)
	return cursor
}

func (s *Service) saveCursor(ctx context.Context, id, target string, cursor int64) error {
	_, err := s.store.DB().ExecContext(ctx, `INSERT INTO replication_job_cursors(job_id,target,cursor) VALUES(?,?,?) ON CONFLICT(job_id,target) DO UPDATE SET cursor=excluded.cursor`, id, target, cursor)
	return err
}

func (s *Service) recordJobStats(ctx context.Context, id string, stats SyncStats, cursor int64, rounds int) error {
	_, err := s.store.DB().ExecContext(ctx, `UPDATE replication_jobs SET stored=stored+?,sent=sent+?,skipped=skipped+?,refused=refused+?,cursor=?,rounds=rounds+?,updated_at=? WHERE id=?`, stats.Stored, stats.Sent, stats.Duplicates, stats.Refused+stats.Rejected, cursor, rounds, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("replication: record job stats: %w", err)
	}
	return nil
}

func parseJobFilter(raw string) (event.Filter, error) {
	if raw == "" {
		return event.Filter{Tags: map[string][]string{}}, nil
	}
	if filter, err := event.ParseFilter([]byte(raw)); err == nil {
		return filter, nil
	} else {
		return event.Filter{}, fmt.Errorf("replication: invalid job filter: %w", err)
	}
}

func safeArtifactPath(root, name string) (string, error) {
	if root == "" || filepath.IsAbs(name) {
		return "", errors.New("replication: invalid artifact path")
	}
	path := filepath.Join(root, name)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("replication: artifact path escapes tenant data directory")
	}
	return path, nil
}
