package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// Execute covers record-owned management operations.
func (s *Service) Execute(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
	p := s.policy()
	if actor != "" && actor != p.Owner {
		return nil, errors.New("restricted: owner record operation")
	}
	now := time.Now().Unix()
	_ = s.audit(ctx, actor, method, "", "", now)
	switch method {
	case "successionstatus":
		var seen, warning, last int64
		_ = s.store.GetSetting(ctx, "records.owner_seen", &seen)
		_ = s.store.GetSetting(ctx, "records.succession_warning", &warning)
		_ = s.store.GetSetting(ctx, "records.succession_notify", &last)
		var log []successionLog
		_ = s.store.GetSetting(ctx, "records.succession_log", &log)
		var warningState any
		if warning > 0 {
			warningState = map[string]any{"since": warning, "lastNotified": last}
		}
		return map[string]any{"ownerSeenAt": seen, "warning": warningState, "handoverAt": func() int64 {
			if warning > 0 {
				return warning + 30*86400
			}
			if p.Succession != nil {
				return seen + int64(p.Succession.AfterDays)*86400 + 30*86400
			}
			return 0
		}(), "log": log, "succession": p.Succession}, nil
	case "setsuccession":
		if len(params) == 0 {
			return nil, errors.New("invalid: missing succession")
		}
		var v policy.Succession
		if err := json.Unmarshal(params[0], &v); err != nil {
			return nil, err
		}
		if v.AfterDays != 90 && v.AfterDays != 180 && v.AfterDays != 365 {
			return nil, errors.New("invalid: succession delay")
		}
		if s.setPolicy != nil {
			next := p
			next.Succession = &v
			next.Notify.Succession = true
			if err := s.setPolicy(next); err != nil {
				return nil, err
			}
		}
		return v, s.store.PutSetting(ctx, "records.succession", v)
	case "clearsuccession":
		if s.setPolicy != nil {
			next := p
			next.Succession = nil
			next.Notify.Succession = false
			if err := s.setPolicy(next); err != nil {
				return nil, err
			}
		}
		return true, s.store.PutSetting(ctx, "records.succession", nil)
	case "listpins":
		return s.store.DB().QueryContext(ctx, `SELECT ref FROM records_pins ORDER BY position`)
	case "pinevent", "unpinevent":
		if len(params) == 0 {
			return nil, errors.New("invalid: missing pin")
		}
		var ref string
		if err := json.Unmarshal(params[0], &ref); err != nil {
			return nil, err
		}
		var refs []string
		rows, _ := s.store.DB().QueryContext(ctx, `SELECT ref FROM records_pins ORDER BY position`)
		for rows.Next() {
			var x string
			_ = rows.Scan(&x)
			if x != ref {
				refs = append(refs, x)
			}
		}
		_ = rows.Close()
		if method == "pinevent" {
			refs = append(refs, ref)
		}
		return s.SetPins(ctx, refs, now)
	case "listviews":
		return s.ViewSummaries(ctx)
	case "notifytest":
		return true, s.notify(ctx, "test", p.Owner, p.Owner, now)
	case "notifystatus":
		return map[string]any{"reports": p.Notify.Reports, "jobs": p.Notify.Jobs, "succession": p.Notify.Succession, "digest": p.Notify.Digest}, nil
	default:
		return nil, fmt.Errorf("unsupported: records method %q", method)
	}
}

func (s *Service) Clock(ctx context.Context) (int64, error) {
	var v int64
	err := s.store.GetSetting(ctx, "records.clock", &v)
	return v, err
}

func parseIntSetting(ctx context.Context, s *storage.Store, key string) int64 {
	var v int64
	_ = s.GetSetting(ctx, key, &v)
	return v
}
