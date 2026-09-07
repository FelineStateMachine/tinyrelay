package records

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestProtocolSigningDoesNotWaitForProjectionLockOrAdvanceClock(t *testing.T) {
	s := &Service{secret: strings.Repeat("0", 63) + "1"}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		for range 80 {
			proof, err := s.SignNIP98(ctx, "GET", "https://relay.test/repo", "", 1234567890)
			if err != nil {
				result <- err
				return
			}
			if proof.CreatedAt != 1234567890 {
				result <- context.DeadlineExceeded
				return
			}
			if err := event.Validate(proof); err != nil {
				result <- err
				return
			}
		}
		result <- nil
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("protocol signing waited for projection lock")
	}
}
