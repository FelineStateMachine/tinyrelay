package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/configport"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/records"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/webui"
)

// initServices constructs the doors around a tenant's single durable store.
// Each service receives the same policy snapshot function so policy changes
// take effect without reopening the tenant.
func (t *Tenant) initServices(ctx context.Context) error {
	var err error
	t.blobs, err = blob.New(ctx, blob.Config{
		Root: t.meta.Paths.Root, Store: t.store, Authorize: t.authorizeBlob,
		CanRead: func(ctx context.Context, pubkey string, hashes []string) bool {
			p := t.Policy()
			if p.Reads == "open" {
				return true
			}
			role, err := t.community.Role(ctx, pubkey)
			return err == nil && role != ""
		}, IsOwner: func(pubkey string) bool { return pubkey == t.Policy().Owner },
	})
	if err != nil {
		return err
	}
	t.sites, err = sites.New(sites.Config{
		Store: t.store,
		GetBlob: func(ctx context.Context, hash string) (sites.Blob, error) {
			entry, body, err := t.blobs.Get(ctx, hash)
			if err != nil {
				return sites.Blob{}, err
			}
			return sites.Blob{Body: body, Type: entry.Type, Size: entry.Size}, nil
		},
		PutBlob: func(ctx context.Context, hash, typ string, body io.Reader) error {
			_, err := t.blobs.Put(ctx, blob.PutOptions{Reader: body, Type: typ, Uploader: t.Policy().Owner, Hash: hash})
			return err
		}, Policy: t.Policy, BaseDomain: hostName(t.publicURL), ReadAccess: func(r *http.Request) bool { return t.siteReadAccess(r) },
	})
	if err != nil {
		return err
	}
	t.records, err = records.New(ctx, records.Config{Store: t.store, Policy: t.Policy, RelayURL: t.RelayURL(), OnGenerated: t.generatedRecord})
	if err != nil {
		return err
	}
	t.config = configport.New(configport.ConfigStore{Store: t.store, Community: t.community, Policy: t.Policy, OnApplied: func(p policy.Policy) { t.replacePolicy(p) }})
	transport := &replication.NostrTransport{Dialer: replication.WebsocketDialer{}}
	t.replication, err = replication.NewService(replication.Config{Store: t.store, Policy: replication.Policy{Enabled: t.Policy().Delivery.Enabled, SelfPubKey: t.Policy().Owner}, CurrentPolicy: func() replication.Policy {
		p := t.Policy()
		return replication.Policy{Enabled: p.Delivery.Enabled, SelfPubKey: p.Owner}
	}, Delivery: transport, Pull: transport, Push: transport, DataDir: t.meta.Paths.Root, Ingest: t.ingest})
	if err != nil {
		return err
	}
	t.git, err = gitrelay.New(gitrelay.Config{Store: t.store, Root: t.meta.Paths.Git, Policy: t.Policy, PublicURL: t.publicURL, AuthorizeHTTP: t.authorizeGit})
	if err != nil {
		return err
	}
	t.ui, err = webui.New(backend{tenant: t}, webui.Options{Actor: t.resolveUIActor})
	if err != nil {
		return err
	}
	t.workCtx, t.workCancel = context.WithCancel(context.Background())
	t.workWG.Add(1)
	go func() { defer t.workWG.Done(); _ = t.replication.Run(t.workCtx) }()
	return nil
}

// backend adapts the management method signature to webui's form-oriented API.
type backend struct{ tenant *Tenant }

func (b backend) Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	return b.tenant.Execute(ctx, actor, method, params)
}
func (b backend) Policy() policy.Policy { return b.tenant.Policy() }
func (b backend) URL() string           { return b.tenant.publicURL }
func (b backend) Slug() string          { return b.tenant.meta.Name }
func (b backend) Identity() string      { return b.tenant.records.PublicKey() }

func (t *Tenant) closeServices(ctx context.Context) error {
	if t.workCancel != nil {
		t.workCancel()
	}
	t.workWG.Wait()
	return nil
}

func hostName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "localhost"
	}
	if u.Hostname() == "" {
		return "localhost"
	}
	return u.Hostname()
}
func (t *Tenant) siteReadAccess(r *http.Request) bool {
	if t.Policy().Reads == "open" {
		return true
	}
	actor, err := t.resolveUIActor(r)
	return err == nil && actor != ""
}
func (t *Tenant) authorizeBlob(r *http.Request, action blob.Action) (string, error) {
	e, err := t.auth.WhoAsks(r.Header.Get("Authorization"), t.publicURL+r.URL.RequestURI(), r.Method, "", string(action), strings.TrimPrefix(r.URL.Path, "/"))
	return first(e), err
}
func (t *Tenant) authorizeGit(ctx context.Context, r *http.Request, repo gitrelay.Repository) error {
	_, err := t.auth.VerifyNIP98(r.Header.Get("Authorization"), t.publicURL+r.URL.RequestURI(), r.Method, "")
	return err
}
func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
func (t *Tenant) resolveUIActor(r *http.Request) (string, error) {
	e, err := t.auth.VerifyNIP98(r.Header.Get("Authorization"), t.publicURL+r.URL.RequestURI(), r.Method, "")
	if err != nil {
		return "", err
	}
	return e.PubKey, nil
}
func (t *Tenant) replacePolicy(p policy.Policy) {
	t.mu.Lock()
	t.policy = p
	t.mu.Unlock()
	t.router.CloseSubscriptions("blocked: relay policy changed; subscribe again")
}
func (t *Tenant) generatedRecord(ctx context.Context, e event.Event) error {
	_, err := t.store.Save(ctx, e, storage.SaveOptions{Now: e.CreatedAt, SearchMode: t.Policy().Features.Search})
	if err == nil {
		t.router.BroadcastGenerated(e)
	}
	return err
}
func (t *Tenant) ingest(ctx context.Context, e event.Event, _ replication.Origin) error {
	_, err := t.store.Save(ctx, e, storage.SaveOptions{Now: e.CreatedAt, SearchMode: t.Policy().Features.Search})
	return err
}
