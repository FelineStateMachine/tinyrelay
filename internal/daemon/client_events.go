package daemon

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type metadataProjection struct {
	next    policy.Policy
	changed bool
}

func (t *Tenant) publishReport(ctx context.Context, e event.Event) (string, error) {
	target := ""
	targetType := ""
	reportType := ""
	for _, tag := range e.Tags {
		if len(tag) > 1 && tag[0] == "e" && target == "" {
			target = tag[1]
			targetType = "event"
			if len(tag) > 2 {
				reportType = tag[2]
			}
		}
		if len(tag) > 1 && tag[0] == "p" && target == "" {
			target = tag[1]
			targetType = "pubkey"
			if len(tag) > 2 {
				reportType = tag[2]
			}
		}
		if len(tag) > 1 && tag[0] == "x" && target == "" {
			target = tag[1]
			targetType = "blob"
			if len(tag) > 2 {
				reportType = tag[2]
			}
		}
	}
	if target == "" {
		return "", errors.New("invalid: report needs an e or p tag")
	}
	if err := t.community.SubmitReportTarget(ctx, e.PubKey, target, targetType, reportType, e.Content, t.Policy().ReportThreshold); err != nil {
		return "", err
	}
	return "info: report received", nil
}

func (t *Tenant) prepareClientEvent(ctx context.Context, e event.Event, s relay.Session, now int64) (eventCommitPlan, *metadataProjection, error) {
	metadata := &metadataProjection{}
	plan, err := t.prepareStoredEvent(ctx, e, replication.OriginClient, now)
	if err != nil {
		return plan, metadata, err
	}
	opts := plan.options
	if (e.Kind == event.KIND_EDIT_METADATA || e.Kind == event.KIND_PINS) && t.roomScope(e) == "" {
		role, roleErr := t.community.Role(ctx, e.PubKey)
		if roleErr != nil {
			return plan, metadata, roleErr
		}
		if role != "owner" && role != "moderator" {
			return plan, metadata, errors.New("restricted: not a group admin")
		}
		opts.AddBeforeCommit(func(txCtx context.Context, tx *sql.Tx) error {
			return t.community.HandleProjectionEventTx(txCtx, tx, e, func(sideTx *sql.Tx) error {
				if e.Kind == event.KIND_EDIT_METADATA {
					metadata.next = t.Policy()
					for _, tag := range e.Tags {
						if len(tag) < 2 {
							continue
						}
						switch tag[0] {
						case "name":
							metadata.next.Name = tag[1][:min(200, len(tag[1]))]
						case "about":
							metadata.next.Description = tag[1][:min(2000, len(tag[1]))]
						case "picture":
							metadata.next.Icon = tag[1][:min(2000, len(tag[1]))]
						}
					}
					metadata.changed = true
					return storage.PutSetting(txCtx, sideTx, "policy", metadata.next)
				}
				pins, err := parsePinTags(e.Tags)
				if err != nil {
					return err
				}
				return t.records.ReplacePinsTx(txCtx, sideTx, pins)
			})
		})
	}
	opts.Intents = append(opts.Intents, t.replication.Prepare(e, replication.OriginClient)...)
	if e.Kind == event.KIND_MARMOT_GROUP {
		principal, principalErr := t.gate.Principal(ctx, e, s)
		if principalErr != nil {
			return plan, metadata, principalErr
		}
		opts.AddBeforeCommit(func(txCtx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(txCtx, `INSERT OR REPLACE INTO marmot_principals(event_id,pubkey) VALUES(?,?)`, e.ID, principal)
			return err
		})
	}
	plan.options = opts
	return plan, metadata, nil
}

func (t *Tenant) persistClientEvent(ctx context.Context, e event.Event, opts storage.SaveOptions) (string, error) {
	var err error
	persist := func(tx *sql.Tx) error {
		_, saveErr := storage.SaveTx(ctx, tx, e, opts)
		return saveErr
	}
	// reason is the OK message; an access request tells the asker it waits,
	// and a member asking again is told so.
	reason := ""
	room := t.roomScope(e)
	switch {
	case room != "" && community.RoomAdminKind(e.Kind):
		_, err = t.community.HandleRoomEventTx(ctx, e, persist)
	case room != "" && !event.IsEphemeral(e.Kind):
		err = t.community.HandleRoomMessageTx(ctx, e, persist)
	case e.Kind == event.KIND_JOIN, e.Kind == event.KIND_LEAVE, e.Kind == event.KIND_NIP43_JOIN, e.Kind == event.KIND_NIP43_LEAVE:
		var membership community.MembershipResult
		membership, err = t.community.HandleMembershipEventTx(ctx, e, persist)
		if err == nil && (membership.AccessRequest || strings.HasPrefix(membership.Message, "duplicate:")) {
			reason = membership.Message
		}
		if err == nil && membership.NewRequest {
			t.notifyJoinRequest(ctx, e)
		}
	case e.Kind == event.KIND_PUT_USER, e.Kind == event.KIND_REMOVE_USER, e.Kind == event.KIND_DELETE_EVENT, e.Kind == event.KIND_CREATE_INVITE:
		_, err = t.community.HandleModerationEventTx(ctx, e, persist)
	default:
		_, err = t.store.Save(ctx, e, opts)
	}
	return reason, err
}
