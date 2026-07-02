// Validates that receiver link teardown is bounded by LinkCloseTimeout
// (finding 3) so a graceful shutdown cannot hang the Run goroutine on an
// unresponsive broker.
package amqp10

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// recordingLink is a linkReceiver double that records the context handed
// to Close, letting tests assert the detach is deadline-bounded.
type recordingLink struct {
	mu            sync.Mutex
	closeCalls    int
	closeHadDDL   bool
	closeDeadline time.Time
}

func (l *recordingLink) Receive(ctx context.Context, _ *slog.Logger, _ ports.MetricsExporter, _ clock.Clock) (*Delivery, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (l *recordingLink) Close(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeCalls++
	l.closeDeadline, l.closeHadDDL = ctx.Deadline()
	return nil
}

// TestReceiver_CloseLink_BoundedByLinkCloseTimeout proves the finding-3
// fix: closeLink hands link.Close a context with a deadline derived from
// SessionOptions.LinkCloseTimeout, rather than an unbounded
// context.Background().
func TestReceiver_CloseLink_BoundedByLinkCloseTimeout(t *testing.T) {
	const linkCloseTimeout = 123 * time.Millisecond
	sess := NewSession(
		SessionOptions{Address: "amqp://localhost", LinkCloseTimeout: linkCloseTimeout},
		connectivity.SessionEphemeral, slog.Default())

	r, err := NewReceiver(ReceiverConfig{Address: "queue/close"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	link := &recordingLink{}
	r.mu.Lock()
	r.link = link
	r.mu.Unlock()

	r.closeLink()

	link.mu.Lock()
	defer link.mu.Unlock()
	if link.closeCalls != 1 {
		t.Fatalf("link.Close called %d times, want 1", link.closeCalls)
	}
	if !link.closeHadDDL {
		t.Fatal("closeLink must pass a bounded (deadline) context to link.Close; " +
			"an unbounded context.Background() can hang Run on shutdown")
	}
	// The deadline must reflect LinkCloseTimeout (~123ms), not the 5s
	// default and certainly not infinity.
	remaining := time.Until(link.closeDeadline)
	if remaining <= 0 || remaining > linkCloseTimeout+500*time.Millisecond {
		t.Fatalf("close deadline = %v from now, want ~%v (bounded by LinkCloseTimeout)",
			remaining, linkCloseTimeout)
	}
}

// TestReceiver_LinkCloseTimeout_DefaultFallback verifies the helper falls
// back to defaultLinkCloseTimeout when the session does not supply one.
func TestReceiver_LinkCloseTimeout_DefaultFallback(t *testing.T) {
	// Directly-constructed receiver with no session.
	r := &Receiver{}
	if got := r.linkCloseTimeout(); got != defaultLinkCloseTimeout {
		t.Fatalf("linkCloseTimeout (no session) = %v, want %v", got, defaultLinkCloseTimeout)
	}

	// Session present but LinkCloseTimeout unset before applyDefaults:
	// NewSession runs applyDefaults so the live value is 5s.
	sess := NewSession(SessionOptions{Address: "amqp://localhost"},
		connectivity.SessionEphemeral, slog.Default())
	r2 := &Receiver{session: sess}
	if got := r2.linkCloseTimeout(); got != 5*time.Second {
		t.Fatalf("linkCloseTimeout (defaulted session) = %v, want 5s", got)
	}
}
