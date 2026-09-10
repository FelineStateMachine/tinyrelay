package daemon

import (
	"net/http"
	"sync"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/community"
	"github.com/FelineStateMachine/tinyrelay/internal/gates"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/telemetry"
)

// callbackService owns callback registration, matching and durable delivery.
// Its dependencies are explicit so callback state does not enlarge Tenant.
type callbackService struct {
	store     *storage.Store
	community *community.Service
	gate      *gates.Gate
	policy    func() policy.Policy
	publicURL string
	relayURL  string
	tenant    catalog.Tenant
	telemetry *telemetry.Telemetry

	callbackMu     sync.Mutex
	callbackIndex  map[int][]callbackRecord
	callbackLocks  map[string]*sync.Mutex
	callbackClient *http.Client
}

type callbackServiceConfig struct {
	Store     *storage.Store
	Community *community.Service
	Gate      *gates.Gate
	Policy    func() policy.Policy
	PublicURL string
	RelayURL  string
	Tenant    catalog.Tenant
	Telemetry *telemetry.Telemetry
}

func newCallbackService(cfg callbackServiceConfig) *callbackService {
	return &callbackService{
		store: cfg.Store, community: cfg.Community, gate: cfg.Gate,
		policy: cfg.Policy, publicURL: cfg.PublicURL, relayURL: cfg.RelayURL,
		tenant: cfg.Tenant, telemetry: cfg.Telemetry,
	}
}
