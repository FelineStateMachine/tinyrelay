package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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
		CanRead: func(ctx context.Context, hash string, pubkeys []string) bool {
			p := t.Policy()
			if p.Reads == "open" {
				return true
			}
			for _, pubkey := range pubkeys {
				if p.Reads == "auth" {
					return true
				}
				role, err := t.community.Role(ctx, pubkey)
				if err == nil && role != "" {
					return true
				}
			}
			return false
		}, IsOwner: func(pubkey string) bool { return pubkey == t.Policy().Owner },
	})
	if err != nil {
		return err
	}
	t.community.ConfigurePolicy(t.Policy)
	t.community.OnMembershipApplied(func(event.Event) {
		go func() {
			generated, err := t.records.PublishMembership(context.Background(), time.Now().Unix())
			if err != nil {
				return
			}
			for _, record := range generated {
				_ = t.generatedRecord(context.Background(), record)
			}
		}()
	})
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
	t.replication, err = replication.NewService(replication.Config{Store: t.store, Policy: replication.Policy{Enabled: t.Policy().Delivery.Enabled, SelfPubKey: t.records.PublicKey()}, CurrentPolicy: func() replication.Policy {
		p := t.Policy()
		return replication.Policy{Enabled: p.Delivery.Enabled, ReadMembersOnly: p.Reads != "open", SelfPubKey: t.records.PublicKey()}
	}, Directory: localDirectory{store: t.store}, Delivery: transport, Pull: transport, Push: transport, DataDir: t.meta.Paths.Root, Ingest: t.ingest})
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

type localDirectory struct{ store *storage.Store }

func (d localDirectory) WriteRelays(pubkey string) []string { return d.relays(pubkey, true) }
func (d localDirectory) ReadRelays(pubkey string) []string  { return d.relays(pubkey, false) }
func (d localDirectory) relays(pubkey string, write bool) []string {
	rows, err := d.store.Query(context.Background(), event.Filter{Authors: []string{pubkey}, Kinds: []int{10002}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err != nil || len(rows.Events) == 0 {
		return nil
	}
	result := make([]string, 0)
	for _, tag := range rows.Events[0].Tags {
		if len(tag) < 2 || tag[0] != "r" || (!write && len(tag) > 2 && tag[2] != "read") || (write && len(tag) > 2 && tag[2] == "read") {
			continue
		}
		result = append(result, tag[1])
	}
	return result
}

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
	pubkey := first(e)
	if err != nil {
		return "", err
	}
	if pubkey == "" {
		return "", errors.New("auth-required: blob authorization required")
	}
	if action == blob.ActionUpload || action == blob.ActionMirror {
		if !t.Policy().Features.Files {
			return "", errors.New("restricted: file uploads are disabled")
		}
		role, roleErr := t.community.Role(r.Context(), pubkey)
		if roleErr != nil || (t.Policy().Writes != "open" && role == "") {
			return "", errors.New("restricted: file upload is not allowed")
		}
	}
	return pubkey, nil
}
func (t *Tenant) authorizeGit(ctx context.Context, r *http.Request, repo gitrelay.Repository) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	e, err := t.auth.VerifyNIP98(r.Header.Get("Authorization"), t.publicURL+r.URL.RequestURI(), r.Method, string(body))
	if err != nil {
		return err
	}
	if e.PubKey == repo.Owner {
		return nil
	}
	role, roleErr := t.community.Role(ctx, e.PubKey)
	if roleErr != nil || (role != "owner" && role != "moderator" && role != "member") {
		return errors.New("restricted: repository authorization required")
	}
	return nil
}
func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
func (t *Tenant) resolveUIActor(r *http.Request) (string, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	e, err := t.auth.VerifyNIP98(r.Header.Get("Authorization"), t.publicURL+r.URL.RequestURI(), r.Method, string(body))
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
