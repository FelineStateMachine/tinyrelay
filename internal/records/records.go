// Package records owns the durable records that a relay signs about itself.
// It deliberately keeps the relay secret inside the tenant settings table and
// exposes only public keys and generated events to callers.
package records

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/communityread"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type Config struct {
	Store               *storage.Store
	Policy              func() policy.Policy
	SetPolicy           func(policy.Policy) error
	RelayURL            string
	GroupID             string
	OnGenerated         func(context.Context, event.Event) error
	DeliverNotification func(context.Context, event.Event, string) error
	// PushNotification receives a plaintext summary of each notification for
	// device delivery. The gift wrap itself is opaque to the relay once sealed.
	PushNotification func(context.Context, string, string, string, string) error
	OnTransfer       func(context.Context, string, string) error
	// Community supplies membership and moderation projections. Keeping these
	// reads behind callbacks prevents records from depending on community's
	// table layout. A nil reader is reserved for isolated records use; hosts
	// serving community projections must provide it.
	Community CommunityReader
}

// CommunityReader is the read-only boundary records needs from community.
// Implementations own the community schema and may use a transaction-backed
// snapshot when the host is publishing a projection.
type CommunityReader interface {
	Members(context.Context) ([]communityread.Member, error)
	ModerationCounts(context.Context, int64) (communityread.ModerationCounts, error)
	MemberStatus(context.Context, string) (member bool, memberCount int, err error)
}

type Member = communityread.Member
type ModerationCounts = communityread.ModerationCounts

type Service struct {
	store               *storage.Store
	policy              func() policy.Policy
	setPolicy           func(policy.Policy) error
	relayURL            string
	groupID             string
	secret              string
	onGenerated         func(context.Context, event.Event) error
	deliverNotification func(context.Context, event.Event, string) error
	pushNotification    func(context.Context, string, string, string, string) error
	onTransfer          func(context.Context, string, string) error
	community           CommunityReader
	mu                  sync.Mutex
}

type MembershipChange struct {
	PubKey string
	Added  *bool
	Roles  []string
}

const secretSetting = "records.relay_secret"

const recordsSchema = `
CREATE TABLE IF NOT EXISTS records_audit(
 id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, actor TEXT NOT NULL,
 action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS records_presence(
 pubkey TEXT PRIMARY KEY, seen_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS records_pins(
 position INTEGER PRIMARY KEY, ref TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS records_view_runs(
 name TEXT NOT NULL, at INTEGER NOT NULL, rows INTEGER NOT NULL);
`

func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("records: nil store")
	}
	if cfg.Policy == nil {
		return nil, errors.New("records: nil policy function")
	}
	if cfg.Community == nil {
		return nil, errors.New("records: nil community reader")
	}
	if _, err := cfg.Store.DB().ExecContext(ctx, recordsSchema); err != nil {
		return nil, fmt.Errorf("records schema: %w", err)
	}
	secret, err := loadOrCreateSecret(ctx, cfg.Store)
	if err != nil {
		return nil, err
	}
	groupID := cfg.GroupID
	if groupID == "" {
		groupID = cfg.RelayURL
	}
	return &Service{store: cfg.Store, policy: cfg.Policy, setPolicy: cfg.SetPolicy, relayURL: cfg.RelayURL, groupID: groupID, secret: secret, onGenerated: cfg.OnGenerated, deliverNotification: cfg.DeliverNotification, pushNotification: cfg.PushNotification, onTransfer: cfg.OnTransfer, community: cfg.Community}, nil
}

var roleOrder = []string{"owner", "moderator", "member", "agent"}
var rolesAbout = map[string]string{"owner": "relay owner", "moderator": "relay moderator", "member": "relay member", "agent": "agent under an owner-signed grant"}

type Presence struct {
	PubKey string `json:"pubkey"`
	SeenAt int64  `json:"seen_at"`
}

type View struct {
	Name  string      `json:"name"`
	Event event.Event `json:"event"`
	Rows  int         `json:"rows"`
}

type ViewRun struct {
	At   int64 `json:"at"`
	Rows int   `json:"rows"`
}

var viewNames = []string{"profiles", "relays", "calendar", "moderation", "articles", "zaps", "presence"}

func calendarStart(e event.Event) int64 {
	s := event.Tag(e, "start")
	if e.Kind == 31922 {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return 0
		}
		return t.Unix()
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
func articlePublished(e event.Event) int64 {
	s := event.Tag(e, "published_at")
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return e.CreatedAt
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func trimString(v any, n int) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}

type successionLog struct {
	At   int64  `json:"at"`
	From string `json:"from"`
	To   string `json:"to"`
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = strconv.IntSize
