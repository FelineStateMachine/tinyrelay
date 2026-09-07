package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

var managementMethods = []string{"supportedmethods", "pullfrom", "pullstatus", "listjobs", "deliverystatus", "addjob", "removejob", "runjob", "backfill", "listdumps", "deletedump", "dumpnow", "backupnow", "listbackups", "deletebackup"}

type JobStatus struct {
	Spec                                                              JobSpec `json:"spec"`
	Phase                                                             string  `json:"phase"`
	Error                                                             string  `json:"error,omitempty"`
	Stored, Sent, Skipped, Refused, Cursor, Rounds, Started, Finished int64
}

func (s *Service) Methods() []string { return append([]string(nil), managementMethods...) }

// Execute accepts decoded arguments for compatibility with existing daemon callers.
func (s *Service) Execute(ctx context.Context, method string, values ...any) (any, error) {
	params := make([]json.RawMessage, len(values))
	for i, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		params[i] = raw
	}
	return s.ExecuteRaw(ctx, method, params)
}

// ExecuteRaw accepts raw JSON arguments, matching NIP-86 and HTTP adapters.
func (s *Service) ExecuteRaw(ctx context.Context, method string, params []json.RawMessage) (any, error) {
	switch method {
	case "supportedmethods":
		return s.Methods(), nil
	case "listjobs":
		return s.ListJobStatus(ctx)
	case "pullstatus":
		return s.pullStatuses(ctx)
	case "deliverystatus":
		return s.deliveryStatuses(ctx)
	case "addjob":
		if len(params) != 1 {
			return nil, errors.New("replication: addjob expects one object")
		}
		var spec JobSpec
		if err := json.Unmarshal(params[0], &spec); err != nil {
			return nil, err
		}
		if err := s.AddJob(ctx, spec); err != nil {
			return nil, err
		}
		return spec, nil
	case "removejob", "runjob":
		id, err := rawString(params)
		if err != nil {
			return nil, err
		}
		if method == "removejob" {
			return true, s.RemoveJob(ctx, id)
		}
		return true, s.RunJob(ctx, id)
	case "pullfrom":
		return s.enqueuePull(ctx, params)
	case "backfill":
		return s.enqueueBackfill(ctx, params)
	case "listdumps":
		return s.listArtifacts(".jsonl")
	case "listbackups":
		return s.listArtifacts(".backup.json")
	case "deletedump":
		return s.deleteArtifact(ctx, params, ".jsonl")
	case "deletebackup":
		return s.deleteArtifact(ctx, params, ".backup.json")
	case "dumpnow":
		return s.enqueueArtifact(ctx, JobDump, params, ".jsonl")
	case "backupnow":
		return s.enqueueArtifact(ctx, JobBackup, params, ".backup.json")
	default:
		return nil, fmt.Errorf("replication: unsupported management method %q", method)
	}
}

func (s *Service) ListJobStatus(ctx context.Context) ([]JobStatus, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT payload,phase,error,stored,sent,skipped,refused,cursor,rounds,started_at,finished_at FROM replication_jobs WHERE state!='cancelled' ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobStatus{}
	for rows.Next() {
		var raw string
		var j JobStatus
		if err := rows.Scan(&raw, &j.Phase, &j.Error, &j.Stored, &j.Sent, &j.Skipped, &j.Refused, &j.Cursor, &j.Rounds, &j.Started, &j.Finished); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &j.Spec); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *Service) pullStatuses(ctx context.Context) ([]JobStatus, error) {
	all, err := s.ListJobStatus(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, j := range all {
		if j.Spec.Kind == JobPull {
			out = append(out, j)
		}
	}
	return out, nil
}

type workStatus struct {
	ID       string `json:"id"`
	Target   string `json:"target"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
}

func (s *Service) deliveryStatuses(ctx context.Context) ([]workStatus, error) {
	rows, err := s.queue.List(ctx, "pending,running")
	if err != nil {
		return nil, err
	}
	out := []workStatus{}
	for _, r := range rows {
		if r.Kind == "delivery" {
			out = append(out, workStatus{r.ID, r.Target, r.State, r.Attempts, r.LastError})
		}
	}
	return out, nil
}
func (s *Service) enqueuePull(ctx context.Context, p []json.RawMessage) (JobSpec, error) {
	if len(p) < 1 || len(p) > 2 {
		return JobSpec{}, errors.New("replication: pullfrom expects a relay URL and optional filter")
	}
	var target string
	if err := json.Unmarshal(p[0], &target); err != nil {
		return JobSpec{}, err
	}
	target, err := normalizePullURL(target)
	if err != nil {
		return JobSpec{}, err
	}
	spec := JobSpec{ID: fmt.Sprintf("pull-%d", time.Now().UnixNano()), Kind: JobPull, Relays: []string{target}}
	if len(p) == 2 && string(p[1]) != "null" {
		var filter map[string]json.RawMessage
		if err := json.Unmarshal(p[1], &filter); err != nil {
			return JobSpec{}, errors.New("replication: pullfrom filter must be an object")
		}
		raw, err := json.Marshal(filter)
		if err != nil {
			return JobSpec{}, err
		}
		spec.Filter = string(raw)
	}
	return spec, s.AddJob(ctx, spec)
}
func (s *Service) enqueueBackfill(ctx context.Context, p []json.RawMessage) (JobSpec, error) {
	if len(p) > 1 {
		return JobSpec{}, errors.New("replication: backfill expects an optional relay list or object")
	}
	var relays []string
	var err error
	if len(p) == 1 {
		if err = json.Unmarshal(p[0], &relays); err != nil {
			var plan struct {
				Relays []string `json:"relays"`
			}
			if objectErr := json.Unmarshal(p[0], &plan); objectErr != nil {
				return JobSpec{}, errors.New("replication: backfill expects a relay list or object")
			}
			relays = plan.Relays
		}
	}
	owner := ""
	if s.config.Owner != nil {
		owner = strings.TrimSpace(s.config.Owner())
	}
	if len(relays) == 0 && owner != "" && s.config.Directory != nil {
		if list, ok := s.config.Directory.(RelayListDiscovery); ok {
			relays = list.RelayList(owner)
		} else {
			relays = append([]string(nil), s.config.Directory.ReadRelays(owner)...)
			for _, relay := range s.config.Directory.WriteRelays(owner) {
				if !containsRelay(relays, relay) {
					relays = append(relays, relay)
				}
			}
		}
	}
	if len(relays) == 0 {
		return JobSpec{}, errors.New("replication: no relay list (kind 10002) is stored here; give relays to fetch from")
	}
	for i, relay := range relays {
		relays[i], err = normalizePullURL(relay)
		if err != nil {
			return JobSpec{}, err
		}
	}
	filter := ""
	if owner != "" {
		raw, marshalErr := json.Marshal(event.Filter{Authors: []string{owner}, Tags: map[string][]string{}})
		if marshalErr != nil {
			return JobSpec{}, fmt.Errorf("replication: encode backfill filter: %w", marshalErr)
		}
		filter = string(raw)
	}
	spec := JobSpec{ID: fmt.Sprintf("backfill-%d", time.Now().UnixNano()), Kind: JobPull, Relays: relays, Filter: filter}
	return spec, s.AddJob(ctx, spec)
}

func containsRelay(relays []string, candidate string) bool {
	for _, relay := range relays {
		if relay == candidate {
			return true
		}
	}
	return false
}

func normalizePullURL(raw string) (string, error) {
	value := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("replication: invalid relay URL")
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", errors.New("replication: invalid relay URL")
	}
	value = strings.TrimRight(u.String(), "/")
	if !safeRelayURL(value) {
		return "", errors.New("replication: invalid relay URL")
	}
	return value, nil
}
func (s *Service) listArtifacts(suffix string) ([]string, error) {
	entries, err := os.ReadDir(s.config.DataDir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
func (s *Service) deleteArtifact(ctx context.Context, p []json.RawMessage, suffix string) (bool, error) {
	name, err := rawString(p)
	if err != nil {
		return false, err
	}
	if !strings.HasSuffix(name, suffix) {
		return false, errors.New("replication: invalid artifact name")
	}
	path, err := safeArtifactPath(s.config.DataDir, name)
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	_, err = s.store.DB().ExecContext(ctx, `INSERT INTO audit(at,actor,action,detail) VALUES(?,?,?,?)`, time.Now().Unix(), "owner", "delete_artifact", name)
	return err == nil, err
}
func (s *Service) enqueueArtifact(ctx context.Context, kind JobKind, p []json.RawMessage, suffix string) (JobSpec, error) {
	name := fmt.Sprintf("%d%s", time.Now().Unix(), suffix)
	if len(p) == 1 {
		var given string
		if err := json.Unmarshal(p[0], &given); err != nil {
			return JobSpec{}, err
		}
		if filepath.Base(given) != given {
			return JobSpec{}, errors.New("replication: invalid artifact name")
		}
		name = given
	}
	spec := JobSpec{ID: fmt.Sprintf("%s-%d", kind, time.Now().UnixNano()), Kind: kind, Relays: []string{name}}
	return spec, s.AddJob(ctx, spec)
}
func rawString(p []json.RawMessage) (string, error) {
	if len(p) != 1 {
		return "", errors.New("replication: expected one string")
	}
	var value string
	if err := json.Unmarshal(p[0], &value); err != nil || strings.TrimSpace(value) == "" {
		return "", errors.New("replication: expected nonempty string")
	}
	return value, nil
}
