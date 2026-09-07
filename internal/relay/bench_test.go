package relay

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func BenchmarkFanout(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		b.Run("clients="+strconv.Itoa(count), func(b *testing.B) {
			r := New(testBackend{}, Config{MaxPendingBytes: 64 << 20})
			clients := make([]*client, 0, count)
			for i := 0; i < count; i++ {
				c := newClient(r, nil, httptest.NewRequest("GET", "http://relay.example/", nil))
				c.subs["all"] = subscription{filters: []event.Filter{{Kinds: []int{1}}}}
				r.clients[c] = struct{}{}
				clients = append(clients, c)
			}
			e := event.Event{ID: strings.Repeat("a", 64), Kind: 1, Content: "benchmark fanout"}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.BroadcastGenerated(e)
				for _, c := range clients {
					c.mu.Lock()
					c.queue = c.queue[:0]
					c.pending = 0
					c.mu.Unlock()
				}
			}
		})
	}
}
