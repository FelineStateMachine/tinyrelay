package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/auth"
	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/configport"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/records"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/webui"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

// initServices constructs the doors around a tenant's single durable store.
// Each service receives the same policy snapshot function so policy changes
// take effect without reopening the tenant.
func (t *Tenant) initServices(ctx context.Context) error {
	var err error
	if err := t.initSessions(ctx); err != nil {
		return err
	}
	t.blobs, err = blob.New(ctx, blob.Config{
		Root: t.meta.Paths.Root, PublicURL: t.publicURL, Store: t.store, Authorize: t.authorizeBlob,
		Limits: func() blob.Limits {
			limits := t.Policy().FileLimits
			return blob.Limits{MaxFileBytes: limits.MaxFileBytes, UserStorageBytes: limits.UserStorageBytes}
		},
		ValidateUpload: t.validateBlobUpload,
		ResolveServers: func(ctx context.Context, pubkey string) ([]string, error) {
			result, err := t.store.Query(ctx, event.Filter{Authors: []string{pubkey}, Kinds: []int{10063}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
			if err != nil {
				return nil, fmt.Errorf("lookup Blossom server list: %w", err)
			}
			if len(result.Events) == 0 {
				return nil, nil
			}
			return blob.BlossomServerList(result.Events[0])
		},
		RecordReport: func(ctx context.Context, tx *sql.Tx, reporter, target, targetType, reportType, content string) error {
			return t.community.SubmitReportTx(ctx, tx, reporter, target, targetType, reportType, content, t.Policy().ReportThreshold)
		},
		CanRead: func(ctx context.Context, hash string, pubkeys []string) bool {
			keys := []string{}
			for _, pubkey := range pubkeys {
				if pubkey == "" {
					continue
				}
				banned, err := t.community.IsBanned(ctx, pubkey)
				if err != nil || banned {
					return false
				}
				keys = append(keys, pubkey)
			}
			_, err := t.gate.Read(ctx, []event.Filter{{}}, relay.Session{PubKeys: keys, RelayURL: t.RelayURL()})
			return err == nil
		}, IsOwner: func(pubkey string) bool { return pubkey == t.Policy().Owner },
	})
	if err != nil {
		return err
	}
	t.community.ConfigurePolicy(t.Policy)
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
		}, Policy: t.Policy, BaseDomain: t.siteDomain(), RelayURL: func(*http.Request) string { return t.RelayURL() }, ResolveHost: t.siteHostResolver, ReadAccess: func(r *http.Request) bool { return t.siteReadAccess(r) },
	})
	if err != nil {
		return err
	}
	t.records, err = records.New(ctx, records.Config{Store: t.store, Policy: t.Policy, RelayURL: t.RelayURL(), GroupID: t.meta.Name, OnGenerated: t.generatedRecord, DeliverNotification: t.deliverNotification, SetPolicy: func(next policy.Policy) error { return t.applyPolicy(context.Background(), next) }})
	if err != nil {
		return err
	}
	t.config = configport.New(configport.ConfigStore{Store: t.store, Community: t.community, Policy: t.Policy, ValidatePolicy: t.validatePolicyTransition, OnApplied: func(p policy.Policy) { t.replacePolicy(p) }})
	transport := &replication.NostrTransport{Dialer: replication.WebsocketDialer{AllowPrivate: t.app.cfg.AllowPrivateRelays, MaxMessageBytes: t.app.cfg.MaxMessageBytes}}
	t.replication, err = replication.NewService(replication.Config{Store: t.store, Owner: func() string { return t.Policy().Owner }, Policy: replication.Policy{Enabled: t.Policy().Delivery.Enabled, SelfPubKey: t.records.PublicKey()}, CurrentPolicy: func() replication.Policy {
		p := t.Policy()
		return replication.Policy{Enabled: p.Delivery.Enabled, ReadMembersOnly: p.Reads != "open", SelfPubKey: t.records.PublicKey(), SelfRelayURL: t.RelayURL()}
	}, Directory: localDirectory{store: t.store}, Discovery: t.ReplicationDiscovery(t.app.cfg.DiscoveryRelays), Delivery: transport, Pull: transport, Push: transport, DataDir: t.meta.Paths.Root, Ingest: t.ingest, BackupProvider: t.ReplicationBackupProvider(),
		ExtraHandlers: t.workHandlers(), WorkMiddleware: t.guardWork, ObserveWork: t.app.telemetry.ObserveWork})
	if err != nil {
		return err
	}
	currentPolicy := t.Policy()
	if err := t.reconcileAutomaticInbox(ctx, currentPolicy, currentPolicy); err != nil {
		return err
	}
	t.git, err = gitrelay.New(gitrelay.Config{Store: t.store, Root: t.meta.Paths.Git, Policy: t.Policy, PublicURL: t.publicURL, AllowPrivateRelays: t.app.cfg.AllowPrivateRelays, PrivatePeers: t.Policy().PrivatePeers, HTTPAuth: t.privateHTTPAuth, GitSync: t.gitSync, EventSync: t.gitEventSync, AuthorizeHTTP: t.authorizeGit, OnPromote: func(ctx context.Context, id string, _ gitrelay.Repository) error {
		return t.releaseGit(ctx, id)
	}})
	if err != nil {
		return err
	}
	t.ui, err = webui.New(backend{tenant: t}, webui.Options{Actor: t.resolveUIActor})
	if err != nil {
		return err
	}
	t.workCtx, t.workCancel = context.WithCancel(context.Background())
	t.workWG.Add(1)
	go func() {
		defer t.workWG.Done()
		if err := t.replication.Run(t.workCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.app.telemetry.Logger().Error("tenant worker stopped", "tenant", t.meta.Name, "error", err)
			t.workErrMu.Lock()
			t.workErr = err
			t.workErrMu.Unlock()
		}
	}()
	t.workWG.Add(1)
	go func() { defer t.workWG.Done(); t.runScheduler() }()
	return nil
}

// backend adapts the management method signature to webui's form-oriented API.
type backend struct{ tenant *Tenant }

type localDirectory struct{ store *storage.Store }

func (d localDirectory) WriteRelays(pubkey string) []string {
	return d.relaysForDirection(pubkey, true)
}
func (d localDirectory) ReadRelays(pubkey string) []string {
	return d.relaysForDirection(pubkey, false)
}
func (d localDirectory) RelayList(pubkey string) []string { return d.relays(pubkey, false, true) }
func (d localDirectory) relaysForDirection(pubkey string, write bool) []string {
	return d.relays(pubkey, write, false)
}
func (d localDirectory) relays(pubkey string, write, all bool) []string {
	rows, err := d.store.Query(context.Background(), event.Filter{Authors: []string{pubkey}, Kinds: []int{10002}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
	if err != nil || len(rows.Events) == 0 {
		return nil
	}
	result := make([]string, 0)
	for _, tag := range rows.Events[0].Tags {
		if len(tag) < 2 || tag[0] != "r" || (!all && !write && len(tag) > 2 && tag[2] != "read") || (!all && write && len(tag) > 2 && tag[2] == "read") {
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

// ReadAllowed is consumed by the web UI private boundary. It deliberately
// delegates to the same uncached membership check used by private Git and
// NIP-42, so a revoked browser session cannot retain page access.
func (b backend) ReadAllowed(ctx context.Context, actor string) error {
	if !b.tenant.PrivateServiceEnabled() {
		return nil
	}
	return b.tenant.requirePrivateAccess(ctx, actor)
}

func (t *Tenant) closeServices(ctx context.Context) error {
	if t.workCancel != nil {
		t.workCancel()
	}
	t.workWG.Wait()
	t.workErrMu.Lock()
	defer t.workErrMu.Unlock()
	return t.workErr
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
	actor, err := t.resolveUIActor(r)
	if err != nil {
		return false
	}
	keys := []string{}
	if actor != "" {
		keys = append(keys, actor)
	}
	_, err = t.gate.Read(r.Context(), []event.Filter{{}}, relay.Session{PubKeys: keys, RelayURL: t.RelayURL()})
	return err == nil
}
func (t *Tenant) authorizeBlob(r *http.Request, action blob.Action) (string, error) {
	hash := ""
	if action == blob.ActionGet || action == blob.ActionDelete || (action == blob.ActionUpload && (r.Method == http.MethodPut || r.Method == http.MethodPatch)) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		path = strings.TrimPrefix(path, "nip96/")
		hash = strings.Split(path, ".")[0]
		if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
			hash = ""
		}
	}
	body, bodyErr := blobAuthBody(r, action)
	if bodyErr != nil {
		return "", bodyErr
	}
	pubkey := ""
	var err error
	if action == blob.ActionUpload && r.Method == http.MethodPost && r.URL.Path == "/nip96" {
		var token event.Event
		token, err = t.auth.VerifyNIP98(r.Header.Get("Authorization"), t.requestURL(r), r.Method, body)
		pubkey = token.PubKey
	} else {
		if auth.IsBlossomAuthorization(r.Header.Get("Authorization")) {
			blossomAction := string(action)
			if action == blob.ActionMirror {
				// BUD-04 mirror uses the upload authorization verb; the fetched
				// bytes are the resulting blob and are scoped by x below.
				blossomAction = string(blob.ActionUpload)
			}
			blobHash := hash
			if blobHash == "" && action == blob.ActionUpload {
				blobHash = strings.ToLower(strings.TrimSpace(r.Header.Get("x-sha-256")))
			}
			var token event.Event
			token, err = t.auth.VerifyBlossomRequest(r.Header.Get("Authorization"), blossomAction, t.siteDomain(), blobHash)
			pubkey = token.PubKey
		} else {
			if isBUD13RemoteUpload(r) {
				var token event.Event
				token, err = t.auth.VerifyNIP98(r.Header.Get("Authorization"), t.requestURL(r), r.Method, "")
				if err == nil && event.Tag(token, "payload") != "" && event.Tag(token, "payload") != emptyPayloadHash() {
					err = errors.New("auth-required: remote upload token payload must hash an empty body")
				}
				pubkey = token.PubKey
			} else {
				var keys []string
				keys, err = t.auth.WhoAsks(r.Header.Get("Authorization"), t.requestURL(r), r.Method, body, string(action), hash)
				pubkey = first(keys)
			}
		}
	}
	if err != nil {
		return "", err
	}
	if pubkey == "" {
		if action == blob.ActionGet {
			return "", nil
		}
		return "", errors.New("auth-required: blob authorization required")
	}
	if banned, banErr := t.community.IsBanned(r.Context(), pubkey); banErr != nil {
		return "", fmt.Errorf("check blob uploader ban: %w", banErr)
	} else if banned {
		return "", errors.New("blocked: this pubkey is banned from this relay")
	}
	if action == blob.ActionUpload || action == blob.ActionMirror {
		if !t.Policy().Features.Files {
			return "", errors.New("restricted: file uploads are disabled")
		}
		role, roleErr := t.community.Role(r.Context(), pubkey)
		if roleErr != nil {
			return "", fmt.Errorf("check blob uploader role: %w", roleErr)
		}
		allowed := t.Policy().Writes == "open" || (t.Policy().Writes == "owner" && role == "owner") || (t.Policy().Writes != "owner" && role != "")
		if !allowed {
			return "", errors.New("restricted: file upload is not allowed")
		}
	}
	return pubkey, nil
}

func (t *Tenant) validateBlobUpload(r *http.Request, hash string) error {
	// PATCH carries only one chunk. Blossom scopes the final path hash, while
	// NIP-98 binds the bytes in this individual request body.
	if r.Method == http.MethodPatch && auth.IsBlossomAuthorization(r.Header.Get("Authorization")) {
		finalHash := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), ".")[0]
		return t.auth.ValidateBlossomPayload(r.Header.Get("Authorization"), string(blob.ActionUpload), finalHash)
	}
	// Mirror proofs bind the HTTP request body before fetching. Blossom
	// authorization additionally names the fetched content through its x tag.
	if r.URL.Path == "/mirror" || isBUD13RemoteUpload(r) {
		if auth.IsBlossomAuthorization(r.Header.Get("Authorization")) {
			return t.auth.ValidateBlossomPayload(r.Header.Get("Authorization"), string(blob.ActionUpload), hash)
		}
		return nil
	}
	return t.auth.ValidateBlobPayload(r.Header.Get("Authorization"), hash)
}

// NIP-98 binds its payload to the complete HTTP body. The streaming Blossom
// upload remains unbuffered and is checked against its resulting SHA-256 by
// ValidateUpload; the JSON and multipart doors are buffered only so their
// signed request can be verified before parsing or fetching any content.
func blobAuthBody(r *http.Request, action blob.Action) (string, error) {
	buffer := action == blob.ActionMirror || action == blob.ActionReport ||
		(action == blob.ActionUpload && r.Method == http.MethodPost && r.URL.Path == "/nip96")
	if !buffer {
		return "", nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", fmt.Errorf("read blob authorization body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return string(body), nil
}

func (t *Tenant) siteDomain() string {
	if t.meta.Name != t.app.cfg.DefaultTenant {
		return t.app.siteBase(t.meta)
	}
	host := hostName(t.publicURL)
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "localhost"
	}
	return host
}
func (t *Tenant) authorizeGit(ctx context.Context, r *http.Request, repo gitrelay.Repository) error {
	if !t.Policy().Features.Grasp {
		return errors.New("restricted: Git hosting is disabled")
	}
	// Authenticate before reading a potentially large pack. The body binding
	// is checked below before any bytes reach git-receive-pack.
	e, ok := privateGitProof(r)
	var err error
	if !ok {
		e, err = t.auth.VerifyNIP98(r.Header.Get("Authorization"), t.requestURL(r), r.Method, "")
		if err != nil {
			return err
		}
	}
	if banned, banErr := t.community.IsBanned(ctx, e.PubKey); banErr != nil {
		return fmt.Errorf("check Git author ban: %w", banErr)
	} else if banned {
		return errors.New("blocked: this pubkey is banned from this relay")
	}
	if privatePolicy(t.Policy()) {
		// Tenant membership is the private-service boundary. A repository
		// maintainer or owner who is not a member must not learn whether a
		// private repository exists through Smart HTTP.
		if err := t.requirePrivateAccess(ctx, e.PubKey); err != nil {
			return err
		}
	} else if e.PubKey != repo.Owner {
		role, roleErr := t.community.Role(ctx, e.PubKey)
		if roleErr != nil || (role != "owner" && role != "moderator" && role != "member") {
			return errors.New("restricted: repository authorization required")
		}
	}
	if _, checked := r.Context().Value(privateGitPayloadCheckedKey{}).(struct{}); checked {
		return nil
	}
	return spoolGitPayload(r, e)
}
func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func isBUD13RemoteUpload(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPut || r.URL.Query().Get("url") == "" {
		return false
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	hash := strings.Split(path, ".")[0]
	return len(hash) == 64 && strings.Trim(hash, "0123456789abcdefABCDEF") == ""
}

func emptyPayloadHash() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}

func (t *Tenant) resolveUIActor(r *http.Request) (string, error) {
	if r.Header.Get("Authorization") == "" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return "", errors.New("auth-required: sign this request")
		}
		return t.cookieActor(r)
	}
	if r.Body == nil {
		r.Body = http.NoBody
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	e, err := t.auth.VerifyNIP98(r.Header.Get("Authorization"), t.requestURL(r), r.Method, string(body))
	if err != nil {
		return "", err
	}
	return e.PubKey, nil
}

func (t *Tenant) workHandlers() map[string]work.Handler {
	handlers := map[string]work.Handler{
		"view-publish": func(ctx context.Context, intent work.Intent) error {
			if err := t.records.MarkView(ctx, intent.Target, time.Now().Unix()); err != nil {
				return err
			}
			t.scheduleViews()
			return nil
		},
		"git-metadata": func(ctx context.Context, intent work.Intent) error {
			var raw string
			err := t.store.DB().QueryRowContext(ctx, "SELECT raw FROM events WHERE id=?", intent.EventID).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			e, err := event.Parse([]byte(raw))
			if err != nil {
				return err
			}
			var repo gitrelay.Repository
			if err := json.Unmarshal([]byte(intent.Payload), &repo); err != nil {
				return err
			}
			if err := t.git.CommitAfterStore(ctx, e, repo); err != nil {
				return err
			}
			if !t.git.IsPending(e.ID) {
				return t.releaseGit(ctx, e.ID)
			}
			return nil
		},
		"catalog-owner": func(ctx context.Context, intent work.Intent) error {
			return t.app.catalog.UpdateOwner(ctx, t.meta.ID, t.Policy().Owner)
		},
		"records-projection": func(ctx context.Context, intent work.Intent) error {
			var payload struct {
				PubKey  string `json:"pubkey"`
				Role    string `json:"role"`
				Removed int    `json:"removed"`
			}
			if err := json.Unmarshal([]byte(intent.Payload), &payload); err != nil {
				return err
			}
			var changes []records.MembershipChange
			if payload.PubKey != "" && (payload.Role != "" || payload.Removed != 0) {
				added := payload.Removed == 0
				changes = []records.MembershipChange{{PubKey: payload.PubKey, Added: &added, Roles: []string{payload.Role}}}
			}
			_, err := t.records.PublishMembershipChanges(ctx, changes, time.Now().Unix())
			return err
		},
		"site-mirror": func(ctx context.Context, intent work.Intent) error {
			return t.sites.RunMirrorIntent(ctx, storage.Intent{Kind: intent.Kind, EventID: intent.EventID, Target: intent.Target, Payload: intent.Payload})
		},
		"callback": t.ReplicationCallbackHandler(),
	}
	for kind, handler := range t.notificationHandlers() {
		handlers[kind] = handler
	}
	return handlers
}

func (t *Tenant) releaseGit(ctx context.Context, id string) error {
	_, err := t.router.CommitGenerated(ctx, func(ctx context.Context) (event.Event, error) {
		var promoted event.Event
		err := t.store.WithTx(ctx, func(tx *sql.Tx) error {
			result, err := tx.ExecContext(ctx, "DELETE FROM pending_events WHERE id=?", id)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil || n == 0 {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM replication_pending_until WHERE id=?", id); err != nil && !isMissingTable(err) {
				return err
			}
			var raw []byte
			err = tx.QueryRowContext(ctx, `SELECT raw FROM events WHERE id=? AND (expires=0 OR expires>?) AND NOT EXISTS(SELECT 1 FROM hidden_events WHERE hidden_events.id=events.id)`, id, time.Now().Unix()).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			promoted, err = event.Parse(raw)
			return err
		})
		return promoted, err
	})
	return err
}

func (t *Tenant) sweep(ctx context.Context, now int64) error {
	if _, err := t.store.SweepExpired(ctx, now); err != nil {
		return err
	}
	opts := storage.RetentionOptions{Now: now, Identity: t.records.PublicKey()}
	rows, err := t.store.DB().QueryContext(ctx, "SELECT kind,days FROM community_retention")
	if err != nil {
		return err
	}
	for rows.Next() {
		var rule storage.RetentionRule
		if err := rows.Scan(&rule.Kind, &rule.Days); err != nil {
			rows.Close()
			return err
		}
		opts.Rules = append(opts.Rules, rule)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	rows, err = t.store.DB().QueryContext(ctx, "SELECT pubkey,keep_days FROM community_members WHERE keep_days>0")
	if err != nil {
		return err
	}
	for rows.Next() {
		var member storage.MemberRetention
		if err := rows.Scan(&member.PubKey, &member.Days); err != nil {
			rows.Close()
			return err
		}
		opts.Members = append(opts.Members, member)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	_, err = t.store.SweepRetention(ctx, opts)
	return err
}
func (t *Tenant) replacePolicy(p policy.Policy) {
	t.mu.Lock()
	previous := t.policy
	t.policy = p
	t.mu.Unlock()
	if err := t.reconcileAutomaticInbox(context.Background(), previous, p); err != nil {
		t.app.telemetry.Logger().Error("reconcile automatic inbox job", "tenant", t.meta.Name, "error", err)
	}
	t.router.CloseSubscriptions("blocked: relay policy changed; subscribe again")
	t.scheduleViews()
}
func (t *Tenant) generatedRecord(ctx context.Context, e event.Event) error {
	ctx, done, admissionErr := t.beginOperation(ctx)
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	_, err := t.router.CommitGenerated(ctx, func(ctx context.Context) (event.Event, error) {
		_, err := t.store.Save(ctx, e, storage.SaveOptions{Now: e.CreatedAt, SearchMode: t.Policy().Features.Search})
		return e, err
	})
	return err
}
func (t *Tenant) ingest(ctx context.Context, e event.Event, _ replication.Origin) error {
	ctx, done, admissionErr := t.beginOperation(ctx)
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	_, err := t.router.CommitGenerated(ctx, func(ctx context.Context) (event.Event, error) {
		return e, t.commitImported(ctx, e, replication.OriginImport)
	})
	if errors.Is(err, storage.ErrDuplicate) || errors.Is(err, storage.ErrReplaced) {
		return nil
	}
	return err
}

func (t *Tenant) commitImported(ctx context.Context, e event.Event, origin replication.Origin) error {
	_ = origin
	if err := t.gate.Import(ctx, e, time.Now().Unix()); err != nil {
		return err
	}
	if err := t.validatePrivateRepositoryPlacement(ctx, e); err != nil {
		return err
	}
	if t.sites != nil {
		if err := sites.ValidateManifest(e); err != nil {
			return err
		}
	}
	opts := storage.SaveOptions{Now: time.Now().Unix(), SearchMode: t.Policy().Features.Search}
	if t.sites != nil {
		opts = t.sites.SaveOptions(e, opts.Now)
		opts.SearchMode = t.Policy().Features.Search
	}
	if t.Policy().Features.Grasp && (e.Kind == event.KIND_REPO || e.Kind == event.KIND_REPO_STATE) {
		repo, err := t.git.ValidateImported(ctx, e)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(repo)
		if err != nil {
			return err
		}
		opts.Intents = append(opts.Intents, storage.Intent{Kind: "git-metadata", EventID: e.ID, Target: repo.Owner + ":" + repo.Identifier, Payload: string(raw)})
		if e.Kind == event.KIND_REPO_STATE {
			before := opts.BeforeCommit
			opts.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				if before != nil {
					if err := before(ctx, tx); err != nil {
						return err
					}
				}
				_, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO pending_events(id,reason) VALUES(?,'git objects')", e.ID)
				return err
			}
		}
	}
	if e.Kind == 30023 {
		opts.Intents = append(opts.Intents, storage.Intent{Kind: "view-publish", EventID: e.ID, Target: "articles", Payload: "{}"})
	}
	_, err := t.store.Save(ctx, e, opts)
	return err
}
