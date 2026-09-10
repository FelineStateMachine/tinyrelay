package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
	"github.com/coder/websocket"
)

type routeBook struct {
	writes map[string][]string
	reads  map[string][]string
}

type countingDiscovery struct{ calls int }

func (d *countingDiscovery) DiscoverRelays(context.Context, string) ([]string, error) {
	d.calls++
	return []string{"wss://remote.example"}, nil
}

type fakeTransport struct {
	result DeliveryResult
	event  event.Event
}

type recordingTransport struct {
	events []event.Event
}

func (r *recordingTransport) Send(_ context.Context, _ string, e event.Event) (DeliveryResult, error) {
	r.events = append(r.events, e)
	return DeliveryResult{Accepted: true}, nil
}

type fakePush struct{ sent map[string][]string }

type countingRoundTripper struct{ calls int }

type fixedResolver []net.IPAddr

func (r fixedResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr(r), nil
}

func TestWebsocketDialerBlocksPrivateByDefault(t *testing.T) {
	_, _, err := (WebsocketDialer{Resolver: fixedResolver{{IP: net.ParseIP("127.0.0.1")}}}).Dial(context.Background(), "ws://relay.test/socket")
	if err == nil || !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("expected private relay rejection, got %v", err)
	}
}

func TestWebsocketDialerAllowsPrivateWhenExplicitlyEnabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		_, _, _ = conn.Read(req.Context())
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	target := "ws://" + parsed.Host
	socket, _, err := (WebsocketDialer{AllowPrivate: true}).Dial(context.Background(), target)
	if err != nil {
		t.Fatalf("allow private dial: %v", err)
	}
	_ = socket.Close(nil)
}

func TestWebsocketDialerRejectsDNSPinningToPrivateAddress(t *testing.T) {
	resolver := fixedResolver{{IP: net.ParseIP("192.168.1.20")}}
	_, _, err := (WebsocketDialer{Resolver: resolver}).Dial(context.Background(), "ws://relay.test/socket")
	if err == nil || !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("expected DNS-pinned private rejection, got %v", err)
	}
}

func TestWebsocketDialerBlocksCGNATAndReservedAddresses(t *testing.T) {
	for _, address := range []string{"100.87.226.30", "198.18.0.1", "240.0.0.1", "224.0.0.1"} {
		resolver := fixedResolver{{IP: net.ParseIP(address)}}
		_, _, err := (WebsocketDialer{Resolver: resolver}).Dial(context.Background(), "ws://relay.test/socket")
		if err == nil || !strings.Contains(err.Error(), "private or local") {
			t.Errorf("address %s was not blocked: %v", address, err)
		}
	}
}

func (r *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
}

type fakeBackupProvider struct{ restored BackupState }

func (f *fakeBackupProvider) Snapshot(context.Context) (BackupState, error) {
	return BackupState{Config: json.RawMessage(`{"owner":"owner"` + `}`), Blobs: []BackupObject{{Name: "blob", SHA256: "hash", Content: []byte("data")}}}, nil
}
func (f *fakeBackupProvider) Restore(_ context.Context, state BackupState) error {
	f.restored = state
	return nil
}

func (f *fakePush) Send(_ context.Context, target string, e event.Event) (DeliveryResult, error) {
	if f.sent == nil {
		f.sent = map[string][]string{}
	}
	f.sent[target] = append(f.sent[target], e.ID)
	return DeliveryResult{Accepted: true}, nil
}

func (f *fakeTransport) Send(_ context.Context, _ string, e event.Event) (DeliveryResult, error) {
	f.event = e
	return f.result, nil
}

func openReplicationStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func signedEvent(t *testing.T, createdAt int64) event.Event {
	t.Helper()
	secret := "1111111111111111111111111111111111111111111111111111111111111111"
	e := event.Event{CreatedAt: createdAt, Kind: 1, Tags: [][]string{}, Content: "hello"}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	return e
}

func (r routeBook) WriteRelays(string) []string { return r.writes["author"] }
func (r routeBook) ReadRelays(string) []string  { return r.reads["recipient"] }

func TestPrepareIntentsPreservesNIP65RoutingAndExclusions(t *testing.T) {
	e := event.Event{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PubKey: "author", Kind: 1}
	// The planner only needs wire identity here; storage validates complete events.
	book := routeBook{writes: map[string][]string{"author": {"wss://write.example"}}, reads: map[string][]string{"recipient": {"wss://read.example"}}}
	policy := Policy{Enabled: true, SelfPubKey: "self", ReadMembersOnly: false}
	got := PrepareIntents(context.Background(), e, OriginClient, policy, book)
	if len(got) != 1 || got[0].Kind != "delivery" || got[0].Target != "wss://write.example" {
		t.Fatalf("intents = %#v", got)
	}
	if got[0].EventID != e.ID {
		t.Fatalf("event id = %q", got[0].EventID)
	}
	for _, excluded := range []event.Event{
		{ID: e.ID, PubKey: e.PubKey, Kind: event.KIND_DM},
		{ID: e.ID, PubKey: e.PubKey, Kind: 1, Tags: [][]string{{"-", "protected"}}},
	} {
		if intents := PrepareIntents(context.Background(), excluded, OriginClient, policy, book); len(intents) != 0 {
			t.Fatalf("excluded event planned: %#v", intents)
		}
	}
}

func TestPrepareIntentsAreDeterministicAndDeduplicated(t *testing.T) {
	e := event.Event{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", PubKey: "author", Kind: 1, Tags: [][]string{{"p", "recipient"}, {"p", "recipient"}}}
	book := routeBook{writes: map[string][]string{"author": {"wss://same.example", "wss://same.example"}}, reads: map[string][]string{"recipient": {"wss://same.example"}}}
	got := PrepareIntents(context.Background(), e, OriginClient, Policy{Enabled: true}, book)
	if len(got) != 1 {
		t.Fatalf("deduped intents = %#v", got)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(got[0].Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["origin"] != string(OriginClient) {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestPrepareIntentsStopsForMembersOnlyPolicy(t *testing.T) {
	e := event.Event{ID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", PubKey: "author", Kind: 1}
	book := routeBook{writes: map[string][]string{"author": {"wss://write.example"}}}
	if got := PrepareIntents(context.Background(), e, OriginClient, Policy{Enabled: true, ReadMembersOnly: true}, book); len(got) != 0 {
		t.Fatalf("members-only intents = %#v", got)
	}
}

func TestRelayDiscoveryPublicationUsesConfiguredOutbox(t *testing.T) {
	e := event.Event{ID: "abababababababababababababababababababababababababababababababab", PubKey: "author", Kind: event.KIND_RELAY_DISCOVERY}
	book := routeBook{writes: map[string][]string{"author": {"wss://operator.example"}}}
	got := PrepareIntents(context.Background(), e, OriginServer, Policy{Enabled: true, SelfPubKey: "author"}, book)
	if len(got) != 1 || got[0].Target != "wss://operator.example" {
		t.Fatalf("peer publication intents = %#v", got)
	}
}

func TestServiceRetainsUnresolvedOutboxRoutingAsDurableWork(t *testing.T) {
	store := openReplicationStore(t)
	service, err := NewService(Config{Store: store, Policy: Policy{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	e := event.Event{ID: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", PubKey: "author", Kind: 1}
	intents := service.Prepare(e, OriginClient)
	if len(intents) != 1 || intents[0].Kind != "delivery-discovery" {
		t.Fatalf("intents = %#v", intents)
	}
	if _, err := service.Queue().Enqueue(context.Background(), work.Intent{Kind: intents[0].Kind, EventID: intents[0].EventID, Target: intents[0].Target, Payload: intents[0].Payload}); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareDoesNotPerformRemoteDiscoveryInPublishPath(t *testing.T) {
	store := openReplicationStore(t)
	discovery := &countingDiscovery{}
	service, err := NewService(Config{Store: store, Policy: Policy{Enabled: true}, Discovery: discovery})
	if err != nil {
		t.Fatal(err)
	}
	e := event.Event{ID: "1212121212121212121212121212121212121212121212121212121212121212", PubKey: "author", Kind: 1}
	intents := service.Prepare(e, OriginClient)
	if len(intents) != 1 || intents[0].Kind != "delivery-discovery" {
		t.Fatalf("intents = %#v", intents)
	}
	if discovery.calls != 0 {
		t.Fatalf("discovery calls during prepare = %d", discovery.calls)
	}
}

func TestServiceWorkersRunIndependentHandlersConcurrently(t *testing.T) {
	store := openReplicationStore(t)
	slowStarted := make(chan struct{})
	fastDone := make(chan struct{})
	service, err := NewService(Config{Store: store, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	handlers := service.Handlers()
	handlers["slow-test"] = func(ctx context.Context, _ work.Intent) error { close(slowStarted); <-ctx.Done(); return ctx.Err() }
	handlers["fast-test"] = func(context.Context, work.Intent) error { close(fastDone); return nil }
	if _, err := service.Queue().Enqueue(context.Background(), work.Intent{Kind: "slow-test", EventID: "slow", Target: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Queue().Enqueue(context.Background(), work.Intent{Kind: "fast-test", EventID: "fast", Target: "two"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- work.RunPool(ctx, service.Queue(), handlers, work.PoolOptions{Workers: 2}) }()
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow handler did not start")
	}
	select {
	case <-fastDone:
	case <-time.After(time.Second):
		t.Fatal("fast handler was blocked by slow handler")
	}
	cancel()
	<-done
}

func TestCallbackHandlerSkipsRevokedRegistration(t *testing.T) {
	store := openReplicationStore(t)
	registration := signedEvent(t, 100)
	registration.Kind = event.KIND_PUSH_REGISTRATION
	registration.Tags = [][]string{{"d", "main"}, {"relay", "wss://relay.example"}, {"callback", "https://push.example"}, {"filter", `{"kinds":[1]}`}}
	if err := event.Sign(&registration, "1111111111111111111111111111111111111111111111111111111111111111"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), registration, storage.SaveOptions{Now: 101}); err != nil {
		t.Fatal(err)
	}
	e := signedEvent(t, 102)
	if _, err := store.Save(context.Background(), e, storage.SaveOptions{Now: 103}); err != nil {
		t.Fatal(err)
	}
	var old PushRegistration
	old, err := ParsePushRegistration(registration, CallbackPolicy{HostOrigins: []string{"https://push.example"}, OwnerOrigins: []string{"https://push.example"}}, "wss://relay.example")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(struct {
		Registration PushRegistration `json:"registration"`
	}{old})
	transport := &countingRoundTripper{}
	handler := NewCallbackHandlerWithPolicy(store, CallbackClient{HTTP: &http.Client{Transport: transport}}, func() CallbackPolicy {
		return CallbackPolicy{HostOrigins: []string{"https://other.example"}, OwnerOrigins: []string{"https://other.example"}}
	}, func() bool { return true }, nil)
	err = handler(context.Background(), work.Intent{Kind: "callback", EventID: e.ID, Target: old.ID, Payload: string(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if transport.calls != 0 {
		t.Fatalf("revoked callback was sent: %d", transport.calls)
	}
}

func TestCallbackHandlerRechecksDeliveredEventVisibility(t *testing.T) {
	store := openReplicationStore(t)
	registration := signedEvent(t, 200)
	registration.Kind = event.KIND_PUSH_REGISTRATION
	registration.Tags = [][]string{{"d", "main"}, {"relay", "wss://relay.example"}, {"callback", "https://push.example"}, {"filter", `{"kinds":[4]}`}}
	if err := event.Sign(&registration, "1111111111111111111111111111111111111111111111111111111111111111"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), registration, storage.SaveOptions{Now: 201}); err != nil {
		t.Fatal(err)
	}
	delivered := signedEvent(t, 202)
	delivered.Kind = 4
	if err := event.Sign(&delivered, "1111111111111111111111111111111111111111111111111111111111111111"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), delivered, storage.SaveOptions{Now: 203}); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParsePushRegistration(registration, CallbackPolicy{HostOrigins: []string{"https://push.example"}, OwnerOrigins: []string{"https://push.example"}}, "wss://relay.example")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(struct {
		Registration PushRegistration `json:"registration"`
	}{parsed})
	transport := &countingRoundTripper{}
	handler := NewCallbackHandlerWithEventVisibility(store, CallbackClient{HTTP: &http.Client{Transport: transport}}, func() CallbackPolicy {
		return CallbackPolicy{HostOrigins: []string{"https://push.example"}, OwnerOrigins: []string{"https://push.example"}}
	}, func() bool { return true }, nil, func(context.Context, string, event.Event) bool { return false })
	if err := handler(context.Background(), work.Intent{Kind: "callback", EventID: delivered.ID, Target: parsed.ID, Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	if transport.calls != 0 {
		t.Fatalf("invisible callback was sent: %d", transport.calls)
	}
}

func TestSafeArtifactPathRejectsTraversal(t *testing.T) {
	if _, err := safeArtifactPath("/tmp/tenant", "../other.json"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestManagementMethodsHaveConcreteDispatch(t *testing.T) {
	store := openReplicationStore(t)
	dataDir := t.TempDir()
	service, err := NewService(Config{Store: store, DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataDir+"/old.jsonl", []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	for _, method := range service.Methods() {
		var params []json.RawMessage
		switch method {
		case "addjob":
			params = []json.RawMessage{json.RawMessage(`{"id":"m1","kind":"dump"}`)}
		case "removejob", "runjob", "deletedump", "deletebackup":
			params = []json.RawMessage{json.RawMessage(`"missing"`)}
		case "pullfrom":
			params = []json.RawMessage{json.RawMessage(`"wss://relay.example"`)}
		case "backfill":
			params = []json.RawMessage{json.RawMessage(`["wss://relay.example"]`)}
		case "dumpnow":
			params = []json.RawMessage{json.RawMessage(`"new.jsonl"`)}
		case "backupnow":
			params = []json.RawMessage{json.RawMessage(`"new.backup.json"`)}
		}
		_, dispatchErr := service.ExecuteRaw(context.Background(), method, params)
		if dispatchErr != nil && strings.Contains(dispatchErr.Error(), "unsupported management method") {
			t.Fatalf("%s has no implementation: %v", method, dispatchErr)
		}
	}
}

func TestBackfillDiscoversOwnerReadRelaysAndFiltersOwner(t *testing.T) {
	store := openReplicationStore(t)
	service, err := NewService(Config{
		Store:     store,
		DataDir:   t.TempDir(),
		Owner:     func() string { return strings.Repeat("a", 64) },
		Directory: testRelayDirectory{reads: []string{"https://history.example"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ExecuteRaw(context.Background(), "backfill", nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := result.(JobSpec)
	if len(spec.Relays) != 1 || spec.Relays[0] != "wss://history.example" {
		t.Fatalf("unexpected discovered relays: %#v", spec.Relays)
	}
	var filter event.Filter
	if err := json.Unmarshal([]byte(spec.Filter), &filter); err != nil {
		t.Fatal(err)
	}
	if len(filter.Authors) != 1 || filter.Authors[0] != strings.Repeat("a", 64) {
		t.Fatalf("unexpected owner filter: %#v", filter)
	}
	if _, err := service.ExecuteRaw(context.Background(), "backfill", []json.RawMessage{json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("empty guided plan should discover relays: %v", err)
	}
}

type testRelayDirectory struct {
	reads []string
	all   []string
}

func (d testRelayDirectory) WriteRelays(string) []string { return nil }
func (d testRelayDirectory) ReadRelays(string) []string  { return append([]string(nil), d.reads...) }
func (d testRelayDirectory) RelayList(string) []string {
	if d.all != nil {
		return append([]string(nil), d.all...)
	}
	return append([]string(nil), d.reads...)
}

func TestBackupProviderStateIsIntegritySealedAndRestored(t *testing.T) {
	store := openReplicationStore(t)
	provider := &fakeBackupProvider{}
	archive, err := CreateBackupWithProvider(context.Background(), store, provider, 100)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadBackup(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.State.Blobs) != 1 {
		t.Fatalf("state = %#v", decoded.State)
	}
	if _, err := RestoreBackupWithProvider(context.Background(), store, provider, decoded, 101); err != nil {
		t.Fatal(err)
	}
	if len(provider.restored.Blobs) != 1 {
		t.Fatal("provider restore was not called")
	}
}

func TestRestoreRollbackRestoresProviderWhenEventWriteFails(t *testing.T) {
	store := openReplicationStore(t)
	ctx := context.Background()
	_, err := store.DB().Exec(`CREATE TRIGGER fail_restore_event BEFORE INSERT ON events WHEN NEW.raw LIKE '%restore-fail%' BEGIN SELECT RAISE(ABORT, 'restore failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	good := signedEvent(t, 1)
	failing := signedEvent(t, 1)
	failing.Content = "restore-fail"
	if err := event.Sign(&failing, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	provider := &fakeBackupProvider{}
	archive := BackupArchive{Format: BackupFormat, Created: 1, Events: []event.Event{good, failing}, State: BackupState{Config: json.RawMessage(`{"owner":"new"}`)}}
	archive, err = SealBackup(archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackupWithProvider(ctx, store, provider, archive, 2); err == nil {
		t.Fatal("restore unexpectedly succeeded")
	}
	if string(provider.restored.Config) != `{"owner":"owner"}` {
		t.Fatalf("provider state was not rolled back: %s", provider.restored.Config)
	}
	rows, err := store.Query(ctx, event.Filter{IDs: []string{good.ID}, Tags: map[string][]string{}}, storage.QueryOptions{Now: 2, Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Events) != 0 {
		t.Fatal("partially inserted event survived rollback")
	}
}

func TestReadCompatibleBackupNormalizesBindWSArchive(t *testing.T) {
	e := signedEvent(t, 1)
	rawEvent, err := event.Canonical(e)
	if err != nil {
		t.Fatal(err)
	}
	encodedEvent, _ := json.Marshal(string(rawEvent))
	legacy := fmt.Sprintf(`{"format":"bind.ws/relay-backup/1","manifest":{"createdAt":1,"archiveSha256":""},"config":{"format":"bind.ws/relay-config/2"},"events":[%s],"blobs":[],"git":[]}`, string(encodedEvent))
	legacy = legacyWithChecksum(legacy)
	archive, err := ReadCompatibleBackup(strings.NewReader(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if archive.Format != BackupFormat || len(archive.Events) != 1 || string(archive.State.Config) != `{"format":"bind.ws/relay-config/2"}` {
		t.Fatalf("archive = %#v", archive)
	}
}

func TestReadCompatibleBackupRequiresLegacyChecksum(t *testing.T) {
	raw := `{"format":"bind.ws/relay-backup/1","manifest":{"createdAt":1,"archiveSha256":""},"config":{"format":"bind.ws/relay-config/2"},"events":[],"blobs":[],"git":[]}`
	if _, err := ReadCompatibleBackup(strings.NewReader(raw)); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("missing checksum accepted: %v", err)
	}
	if _, err := ReadBackup(strings.NewReader(`{"format":"tinyrelay/backup/1"} {}`)); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing native JSON accepted: %v", err)
	}
}

func TestReadCompatibleBackupRetainsLegacyIdentityAndSQLGit(t *testing.T) {
	e := signedEvent(t, 1)
	rawEvent, err := event.Canonical(e)
	if err != nil {
		t.Fatal(err)
	}
	encodedEvent, _ := json.Marshal(string(rawEvent))
	legacy := fmt.Sprintf(`{"format":"bind.ws/relay-backup/1","manifest":{"createdAt":1,"owner":"%s","relayIdentity":"relay-key","slug":"demo","archiveSha256":""},"config":{"format":"bind.ws/relay-config/2"},"events":[%s],"blobs":[],"git":[]}`, e.PubKey, string(encodedEvent))
	legacy = legacyWithChecksum(legacy)
	archive, err := ReadCompatibleBackup(strings.NewReader(legacy))
	if err != nil {
		t.Fatal(err)
	}
	var identity map[string]string
	if err := json.Unmarshal(archive.State.Identity, &identity); err != nil || identity["owner"] != e.PubKey {
		t.Fatalf("identity = %#v, err=%v", identity, err)
	}
	withSQLGit := strings.TrimSuffix(legacy, `}`) + `,"sqlGit":{"format":"bind.ws/git-sqlite/1","repositories":[]}}`
	withSQLGit = legacyWithChecksum(withSQLGit)
	converted, err := ReadCompatibleBackup(strings.NewReader(withSQLGit))
	if err != nil {
		t.Fatal(err)
	}
	if string(converted.State.GitSQL) == "" {
		t.Fatal("SQL Git payload was dropped")
	}
}

func legacyWithChecksum(raw string) string {
	key := `"archiveSha256":"`
	at := strings.Index(raw, key)
	if at < 0 {
		return raw
	}
	start := at + len(key)
	end := strings.IndexByte(raw[start:], '"')
	if end < 0 {
		return raw
	}
	end += start
	unsigned := raw[:start] + raw[end:]
	return raw[:start] + digest([]byte(unsigned)) + raw[end:]
}

func TestPrepareIntentsDoesNotTreatImportedEventsAsLocalOutbox(t *testing.T) {
	e := event.Event{ID: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", PubKey: "author", Kind: 1}
	book := routeBook{writes: map[string][]string{"author": {"wss://write.example"}}}
	got := PrepareIntents(context.Background(), e, OriginImport, Policy{Enabled: true}, book)
	if len(got) != 0 {
		t.Fatalf("import intents = %#v", got)
	}
	_ = storage.Intent{}
}

func TestDeliveryHandlerUsesStoredEventAndAcknowledgesDuplicate(t *testing.T) {
	store := openReplicationStore(t)
	e := signedEvent(t, 1)
	if _, err := store.Save(context.Background(), e, storage.SaveOptions{Now: e.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	transport := &fakeTransport{result: DeliveryResult{Accepted: false, Message: "duplicate: already have this event"}}
	handler := NewDeliveryHandler(store, transport)
	intent := storage.Intent{Kind: "delivery", EventID: e.ID, Target: "wss://relay.example", Payload: `{ "origin": "client" }`}
	if err := handler(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if transport.event.ID != e.ID {
		t.Fatalf("sent event = %#v", transport.event)
	}
}

func TestDeliveryHandlerSendsAuthorRelayListBeforePublicEvent(t *testing.T) {
	store := openReplicationStore(t)
	list := signedEvent(t, 1)
	list.Kind = 10002
	if err := event.Sign(&list, strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	main := signedEvent(t, 1)
	if _, err := store.Save(context.Background(), list, storage.SaveOptions{Now: list.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), main, storage.SaveOptions{Now: main.CreatedAt + 1}); err != nil {
		t.Fatal(err)
	}
	transport := &recordingTransport{}
	handler := NewDeliveryHandler(store, transport)
	if err := handler(context.Background(), storage.Intent{Kind: "delivery", EventID: main.ID, Target: "ws://localhost:7447"}); err != nil {
		t.Fatal(err)
	}
	if len(transport.events) != 2 || transport.events[0].ID != list.ID || transport.events[1].ID != main.ID {
		t.Fatalf("delivery order = %#v", transport.events)
	}
}

func TestSafeRelayURLDefersPrivateAddressPolicyToDialer(t *testing.T) {
	if !safeRelayURL("ws://localhost:7447") {
		t.Fatal("localhost relay URL rejected before operator dial policy")
	}
	if canonicalRelayURL("ws://LOCALHOST:7447/r/main/") != canonicalRelayURL("http://localhost:7447/r/main") {
		t.Fatal("self relay URL aliases did not normalize")
	}
}

func TestPushRegistrationRequiresBothOriginApprovals(t *testing.T) {
	e := event.Event{ID: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", PubKey: "author", Kind: event.KIND_PUSH_REGISTRATION, Tags: [][]string{
		{"d", "main"}, {"relay", "wss://relay.example"}, {"callback", "https://push.example"}, {"filter", `{"kinds":[1]}`}, {"include_event"},
	}}
	reg, err := ParsePushRegistration(e, CallbackPolicy{HostOrigins: []string{"https://push.example"}, OwnerOrigins: []string{"https://push.example"}}, "wss://relay.example")
	if err != nil {
		t.Fatal(err)
	}
	if !PushMatches(reg, event.Event{Kind: 1, Content: "hello"}) || !reg.IncludeEvent {
		t.Fatalf("registration = %#v", reg)
	}
	if _, err := ParsePushRegistration(e, CallbackPolicy{HostOrigins: []string{"https://push.example"}}, "wss://relay.example"); err == nil {
		t.Fatal("unapproved owner origin accepted")
	}
}

func TestImportAndDumpRoundTrip(t *testing.T) {
	store := openReplicationStore(t)
	e := signedEvent(t, 100)
	raw, err := event.Canonical(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportJSONL(context.Background(), store, bytes.NewReader(append(raw, '\n')), 101); err != nil {
		t.Fatal(err)
	}
	var dump bytes.Buffer
	if err := WriteDump(context.Background(), store, &dump, 101); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dump.Bytes(), []byte(e.ID)) {
		t.Fatalf("dump does not contain imported event: %s", dump.String())
	}
}

func TestPushCursorIsDurablePerTarget(t *testing.T) {
	store := openReplicationStore(t)
	e := signedEvent(t, 100)
	if _, err := store.Save(context.Background(), e, storage.SaveOptions{Now: 101}); err != nil {
		t.Fatal(err)
	}
	transport := &fakePush{}
	service, err := NewService(Config{Store: store, Push: transport})
	if err != nil {
		t.Fatal(err)
	}
	spec := JobSpec{ID: "job-1", Kind: JobPush, Relays: []string{"wss://a.example", "wss://b.example"}}
	if err := service.push(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if len(transport.sent["wss://a.example"]) != 1 || len(transport.sent["wss://b.example"]) != 1 {
		t.Fatalf("sent = %#v", transport.sent)
	}
	var first, second int64
	if err := store.DB().QueryRow(`SELECT cursor FROM replication_job_cursors WHERE job_id=? AND target=?`, spec.ID, spec.Relays[0]).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT cursor FROM replication_job_cursors WHERE job_id=? AND target=?`, spec.ID, spec.Relays[1]).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first == 0 || second == 0 {
		t.Fatalf("cursors = %d,%d", first, second)
	}
}
