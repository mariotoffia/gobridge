package paho

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// TestBug_MQTTRES3_DiscardDisconnectContextBounded pins MQTT-RES-3: the context
// used to tear down a ConnectionManager on a failed/abandoned Start path is
// BOUNDED (has a deadline), so a disconnect cannot block forever if the SDK
// ignores cancellation. It prefers ReconnectTimeout, then ConnectTimeout, then
// the default.
func TestBug_MQTTRES3_DiscardDisconnectContextBounded(t *testing.T) {
	cases := []struct {
		name string
		opts SessionOptions
		want time.Duration
	}{
		{"reconnect wins", SessionOptions{ReconnectTimeout: 3 * time.Second, ConnectTimeout: 20 * time.Second}, 3 * time.Second},
		{"connect fallback", SessionOptions{ConnectTimeout: 7 * time.Second}, 7 * time.Second},
		{"default fallback", SessionOptions{}, DefaultConnectTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSession(tc.opts, connectivity.SessionEphemeral, slog.Default())
			ctx, cancel := s.discardDisconnectContext()
			defer cancel()
			dl, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("discard disconnect context has no deadline; teardown could block forever (MQTT-RES-3)")
			}
			// The deadline should be ~want from now; allow generous slack.
			remaining := time.Until(dl)
			if remaining <= 0 || remaining > tc.want+time.Second {
				t.Fatalf("deadline in %v, want ~%v", remaining, tc.want)
			}
		})
	}

	// Sanity: the returned context is a child of Background (independent of any
	// request ctx), so a cancelled caller cannot abort the teardown early.
	s := NewSession(SessionOptions{}, connectivity.SessionEphemeral, slog.Default())
	ctx, cancel := s.discardDisconnectContext()
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("fresh discard context already errored: %v", err)
	}
	_ = context.Background()
}
