package tinyrelay_test

import (
	"context"
	"errors"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
	"github.com/FelineStateMachine/tinyrelay/tinyrelay"
)

type hostBackend struct {
	publishErr error
	published  int
}

func (b *hostBackend) Publish(context.Context, nostr.Event, tinyrelay.Session) (string, error) {
	b.published++
	return "", b.publishErr
}

func (*hostBackend) Query(context.Context, []nostr.Filter, tinyrelay.Session) ([]nostr.Event, error) {
	return nil, nil
}

func (*hostBackend) Count(context.Context, []nostr.Filter, tinyrelay.Session) (any, error) {
	return map[string]any{"count": 0}, nil
}

func TestEmbeddedRelayWaitsForHostCommit(t *testing.T) {
	ctx := context.Background()
	rejected := errors.New("host transaction failed")
	backend := &hostBackend{publishErr: rejected}
	app, err := tinyrelay.New(backend, tinyrelay.Config{RelayURL: "wss://relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	delivered := 0
	stop := app.Listen(func(nostr.Event) { delivered++ })
	defer stop()
	e := nostr.Event{ID: "host-accepted-event", Kind: 1}
	if _, err := app.Publish(ctx, e, tinyrelay.Session{}); !errors.Is(err, rejected) {
		t.Fatalf("publication error: %v", err)
	}
	if delivered != 0 {
		t.Fatal("failed transaction reached live delivery")
	}
	backend.publishErr = nil
	if _, err := app.Publish(ctx, e, tinyrelay.Session{}); err != nil {
		t.Fatal(err)
	}
	if backend.published != 2 || delivered != 1 {
		t.Fatalf("commits=%d, deliveries=%d", backend.published, delivered)
	}
}

func TestEmbeddedRelayRequiresBackend(t *testing.T) {
	if _, err := tinyrelay.New(nil, tinyrelay.Config{}); err == nil {
		t.Fatal("accepted a relay without a backend")
	}
}
