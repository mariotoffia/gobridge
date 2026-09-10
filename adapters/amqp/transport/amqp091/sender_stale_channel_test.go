// ═══════════════════════════════════════════════
// Production-readiness remediation tests: sender stale-channel guard.
//
// Covers Finding #8 — a publisher channel cached across a reconnect must
// be detected as stale and replaced, so the first post-reconnect publish
// does not fail on a dead channel.
// ═══════════════════════════════════════════════
package amqp091

import "testing"

// TestSenderChannelStale exercises the pure staleness predicate that
// drives channel reuse-vs-reopen in ensureChannelLocked.
func TestSenderChannelStale(t *testing.T) {
	connA := newMockConnection()
	connB := newMockConnection()

	tests := []struct {
		name        string
		prevConn    amqpConnection
		currentConn amqpConnection
		closed      bool
		want        bool
	}{
		{"same-open-connection", connA, connA, false, false},
		{"different-connection-after-reconnect", connA, connB, false, true},
		{"same-but-closed-connection", connA, connA, true, true},
		{"nil-previous", nil, connA, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closed := tt.closed
			if mc, ok := tt.currentConn.(*mockConnection); ok {
				mc.IsClosedFn = func() bool { return closed }
			}
			if got := senderChannelStale(tt.prevConn, tt.currentConn); got != tt.want {
				t.Fatalf("senderChannelStale = %v, want %v", got, tt.want)
			}
		})
	}
}
