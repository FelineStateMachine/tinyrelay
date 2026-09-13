package relayapp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
)

func TestStandaloneBackendPublishesAndQueriesSignedEvents(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, Config{DataDir: t.TempDir(), Owner: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	secret := strings.Repeat("1", 64)
	e := event.Event{CreatedAt: time.Now().Unix(), Kind: 1, Tags: [][]string{}, Content: "hello"}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(ctx, e, relay.Session{}); err != nil {
		t.Fatal(err)
	}
	rows, err := b.Query(ctx, []event.Filter{{IDs: []string{e.ID}}}, relay.Session{})
	if err != nil || len(rows) != 1 || rows[0].ID != e.ID {
		t.Fatalf("query = %#v, %v", rows, err)
	}
	if reason, err := b.Publish(ctx, e, relay.Session{}); err != nil || !strings.Contains(reason, "duplicate") {
		t.Fatalf("duplicate = %q, %v", reason, err)
	}
}

func TestStandaloneAcceptsGenericNostrKinds(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	secret := strings.Repeat("1", 64)
	pubkey, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 3)
	for i, kind := range []int{24133, 5000, 4242, 30390} {
		tags := [][]string{}
		if kind == 24133 {
			tags = [][]string{{"p", pubkey}}
		}
		if kind == 30390 {
			tags = [][]string{{"d", "opaque"}}
		}
		e := event.Event{CreatedAt: time.Now().Unix() + int64(i), Kind: kind, Tags: tags, Content: "generic"}
		if err := event.Sign(&e, secret); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Publish(ctx, e, relay.Session{}); err != nil {
			t.Fatalf("kind %d: %v", kind, err)
		}
		if kind == 24133 && !b.CanRead(e, relay.Session{}) {
			t.Fatal("anonymous NIP-46 event was blocked")
		}
		if kind != 24133 {
			ids = append(ids, e.ID)
		}
	}
	rows, err := b.Query(ctx, []event.Filter{{IDs: ids}}, relay.Session{PubKeys: []string{pubkey}})
	if err != nil || len(rows) != len(ids) {
		got := make([]string, len(rows))
		for i, row := range rows {
			got[i] = row.ID
		}
		t.Fatalf("generic query = %d/%d ids=%v want=%v, %v", len(rows), len(ids), got, ids, err)
	}
}

func TestStandaloneNIP70AuthorOnlyPublication(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	secret := strings.Repeat("1", 64)
	e := event.Event{CreatedAt: time.Now().Unix(), Kind: 1, Tags: [][]string{{"-"}}, Content: "protected"}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(ctx, e, relay.Session{}); err == nil {
		t.Fatal("unauthenticated NIP-70 publication accepted")
	}
	pubkey, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(ctx, e, relay.Session{PubKeys: []string{pubkey}}); err != nil {
		t.Fatal(err)
	}
}

func TestStandaloneAuthRequiredAppliesToReadsAndWrites(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, Config{DataDir: t.TempDir(), Owner: strings.Repeat("a", 64), AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	secret := strings.Repeat("1", 64)
	e := event.Event{CreatedAt: time.Now().Unix(), Kind: 1, Tags: [][]string{}, Content: "hello"}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(ctx, e, relay.Session{}); err == nil {
		t.Fatal("unauthenticated publish accepted")
	}
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(ctx, e, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Query(ctx, []event.Filter{{IDs: []string{e.ID}}}, relay.Session{}); err == nil {
		t.Fatal("unauthenticated query accepted")
	}
}

func TestInformationDoesNotAdvertiseHostedExtensions(t *testing.T) {
	b, err := Open(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	info := b.Information()
	for _, n := range info["supported_nips"].([]int) {
		if n == 86 {
			t.Fatal("standalone relay advertises NIP-86")
		}
	}
}

func TestOpenResolvesDataDirectoryAndReopens(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	relative := "relay"
	b, err := Open(context.Background(), Config{DataDir: relative})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(b.cfg.DataDir) {
		t.Fatalf("data directory is not absolute: %q", b.cfg.DataDir)
	}
	if _, err := Open(context.Background(), Config{DataDir: relative}); err == nil {
		t.Fatal("second backend opened the same data directory")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), Config{DataDir: relative})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}
