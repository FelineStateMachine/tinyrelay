package records

import (
	"context"
	"database/sql"
	"errors"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// Tick is called by the host scheduler and does not start a goroutine.
func (s *Service) Tick(ctx context.Context, now int64) error {
	p := s.policy()
	if err := s.tickViews(ctx, p, now); err != nil {
		return err
	}
	if p.Succession == nil || p.Succession.Heir == "" || p.Owner == "" {
		return nil
	}
	// The heir must remain a member while the plan is armed.
	member, memberCount, err := s.memberStatus(ctx, p.Succession.Heir)
	if err != nil {
		return err
	}
	if memberCount > 0 && !member {
		if s.setPolicy != nil {
			next := p
			next.Succession = nil
			if err := s.setPolicy(next); err != nil {
				return err
			}
		}
		_ = s.store.PutSetting(ctx, "records.succession_warning", int64(0))
		_ = s.store.PutSetting(ctx, "records.succession_notify", int64(0))
		return s.notifyText(ctx, "succession", "Your heir is no longer a member, so the handover plan is off. Name another heir if you still want one.", "succession on relay", p.Owner, now)
	}
	var heartbeat int64
	if err := s.store.GetSetting(ctx, "records.owner_seen", &heartbeat); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if heartbeat == 0 {
		heartbeat = now
		if err := s.store.PutSetting(ctx, "records.owner_seen", heartbeat); err != nil {
			return err
		}
		return nil
	}
	delay := int64(p.Succession.AfterDays) * 86400
	if now < heartbeat+delay {
		return nil
	}
	var warned int64
	_ = s.store.GetSetting(ctx, "records.succession_warning", &warned)
	if warned == 0 {
		if err := s.store.PutSetting(ctx, "records.succession_warning", now); err != nil {
			return err
		}
		if err := s.store.PutSetting(ctx, "records.succession_notify", now); err != nil {
			return err
		}
		return s.notifyText(ctx, "succession", "You have been away long enough to start the succession warning period. Any signed action cancels it.", "succession on relay", p.Owner, now)
	}
	warnUntil := warned + 30*86400
	if now < warnUntil {
		var last int64
		_ = s.store.GetSetting(ctx, "records.succession_notify", &last)
		if now-last >= 7*86400 {
			_ = s.store.PutSetting(ctx, "records.succession_notify", now)
			return s.notifyText(ctx, "succession", "Still no sign of you. The relay will go to your heir unless you sign in.", "succession on relay", p.Owner, now)
		}
		return nil
	}
	old, heir := p.Owner, p.Succession.Heir
	if s.setPolicy == nil {
		return errors.New("records: succession policy writer unavailable")
	}
	next := p
	next.Owner = heir
	next.Succession = nil
	if err := s.setPolicy(next); err != nil {
		return err
	}
	if err := appendSuccessionLog(ctx, s.store, successionLog{At: now, From: old, To: heir}); err != nil {
		return err
	}
	_ = s.store.PutSetting(ctx, "records.owner_seen", now)
	_ = s.store.PutSetting(ctx, "records.succession_warning", int64(0))
	_ = s.store.PutSetting(ctx, "records.succession_notify", int64(0))
	if s.onTransfer != nil {
		if err := s.onTransfer(ctx, old, heir); err != nil {
			return err
		}
	}
	if err := s.notifyText(ctx, "succession", "The relay now belongs to your heir, as you planned. You stay on as a moderator.", "succession on relay", old, now); err != nil {
		return err
	}
	return s.notifyText(ctx, "succession", "The relay is yours now. Its owner named you heir and has been away through the warning period.", "succession on relay", heir, now)
}

func appendSuccessionLog(ctx context.Context, st *storage.Store, entry successionLog) error {
	var log []successionLog
	_ = st.GetSetting(ctx, "records.succession_log", &log)
	log = append(log, entry)
	if len(log) > 10 {
		log = log[len(log)-10:]
	}
	return st.PutSetting(ctx, "records.succession_log", log)
}

func (s *Service) tickViews(ctx context.Context, p policy.Policy, now int64) error {
	defaults := map[string]int64{"profiles": 86400, "relays": 86400, "calendar": 3600, "moderation": 86400, "articles": 86400, "zaps": 3600}
	for name, period := range defaults {
		mode := effectiveViewTrigger(p, name)
		if mode == "off" || !viewStored(p, name) {
			continue
		}
		if mode == "daily" {
			period = 86400
		}
		if mode == "hourly" {
			period = 3600
		}
		key := "records.view." + name + ".at"
		var last int64
		if err := s.store.GetSetting(ctx, key, &last); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var dirty int64
		if mode == "write" {
			if err := s.store.GetSetting(ctx, "records.view."+name+".dirty", &dirty); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if dirty != 0 && now-dirty >= 10 {
				// A write burst is coalesced into one publication.
			} else if dirty != 0 {
				continue
			} else if last != 0 && now-last < period-300 {
				continue
			}
		} else if last != 0 && now-last < period-300 {
			continue
		}
		if mode == "hourly" {
			fp, err := s.viewFingerprint(ctx, name, now)
			if err != nil {
				return err
			}
			var previous string
			if err := s.store.GetSetting(ctx, "records.view."+name+".fingerprint", &previous); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if previous != "" && previous == fp {
				if err := s.store.PutSetting(ctx, key, now); err != nil {
					return err
				}
				continue
			}
			if err := s.store.PutSetting(ctx, "records.view."+name+".fingerprint", fp); err != nil {
				return err
			}
		}
		e, err := s.view(ctx, name, policy.Access{Owner: true}, now)
		if err != nil {
			return err
		}
		if err := s.store.PutSetting(ctx, key, now); err != nil {
			return err
		}
		if err := s.recordViewRun(ctx, name, now, maxInt(0, len(e.Tags)-3)); err != nil {
			return err
		}
		if mode == "write" && dirty != 0 {
			if err := s.store.PutSetting(ctx, "records.view."+name+".dirty", int64(0)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) Heartbeat(ctx context.Context, owner string, now int64) error {
	if owner == "" || owner != s.policy().Owner {
		return errors.New("restricted: owner heartbeat")
	}
	if err := s.store.PutSetting(ctx, "records.owner_seen", now); err != nil {
		return err
	}
	_ = s.store.PutSetting(ctx, "records.succession_warning", int64(0))
	return s.store.PutSetting(ctx, "records.succession_notify", int64(0))
}
