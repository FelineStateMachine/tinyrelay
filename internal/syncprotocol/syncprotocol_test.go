package syncprotocol

import (
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestHLLOffsetAndRegisters(t *testing.T) {
	f := event.Filter{Tags: map[string][]string{"d": {"30000:" + strings.Repeat("a", 64) + ":x"}}}
	offset, ok := HLLFilterOffset(f)
	if !ok || offset < 8 || offset > 23 {
		t.Fatalf("unexpected offset %d, ok=%v", offset, ok)
	}
	h := NewHLL(offset)
	if err := h.Add(strings.Repeat("b", 64)); err != nil {
		t.Fatalf("add pubkey: %v", err)
	}
	if len(h.Hex()) != 512 {
		t.Fatalf("HLL must encode 256 registers, got %d hex chars", len(h.Hex()))
	}
	if err := h.MergeHex(h.Hex()); err != nil {
		t.Fatalf("merge own registers: %v", err)
	}
}

func TestNegentropyReconcilesBothDirections(t *testing.T) {
	left, err := NewSession([]Item{{Timestamp: 1, ID: strings.Repeat("a", 64)}, {Timestamp: 2, ID: strings.Repeat("b", 64)}}, true)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewSession([]Item{{Timestamp: 2, ID: strings.Repeat("b", 64)}, {Timestamp: 3, ID: strings.Repeat("c", 64)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := left.Start()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20 && msg != ""; i++ {
		server, err := right.Reconcile(msg)
		if err != nil {
			t.Fatalf("server reconcile: %v", err)
		}
		msg = server.Response
		if msg == "" {
			break
		}
		client, err := left.Reconcile(msg)
		if err != nil {
			t.Fatalf("client reconcile: %v", err)
		}
		msg = client.Response
		if client.Done {
			if len(client.Have) != 1 || client.Have[0] != strings.Repeat("a", 64) || len(client.Need) != 1 || client.Need[0] != strings.Repeat("c", 64) {
				t.Fatalf("unexpected diff: have=%v need=%v", client.Have, client.Need)
			}
			return
		}
	}
	t.Fatal("negentropy did not finish")
}
