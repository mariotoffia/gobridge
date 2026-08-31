package paho

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// The SDK applies its own per-packet acknowledgement deadline INSIDE the
// caller's context (CONNACK, SUBACK, UNSUBACK, PUBACK/PUBCOMP), defaulting to
// 10 seconds. Every adapter-owned budget here is longer than that, so leaving
// the default in place makes a perfectly healthy 12-second SUBACK fail with a
// deadline error and restarts a reconcile that would have converged. The
// adapter therefore hands the SDK a budget no shorter than the longest
// enclosing deadline it could pre-empt.

// TestPacketTimeout_CoversEveryEnclosingBudget pins that the SDK packet budget
// is at least as long as each adapter-owned deadline that wraps a packet
// acknowledgement.
func TestPacketTimeout_CoversEveryEnclosingBudget(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs:       []string{"tcp://192.0.2.1:1883"},
		ClientID:         "packet-budget",
		ConnectTimeout:   45 * time.Second,
		ReconnectTimeout: 20 * time.Second,
		ReconcileTimeout: 90 * time.Second,
	}, connectivity.SessionEphemeral, nil)

	got := s.packetTimeout()

	require.GreaterOrEqual(t, got, 90*time.Second, "must not pre-empt reconcile_timeout")
	require.GreaterOrEqual(t, got, 45*time.Second, "must not pre-empt connect_timeout")
	require.GreaterOrEqual(t, got, 20*time.Second, "must not pre-empt reconnect_timeout")
}

// TestPacketTimeout_CoversConfiguredSenderTimeout pins that a sender budget
// longer than every session budget still governs its own publish: the packet
// deadline the SDK applies inside Send must not cut it short.
func TestPacketTimeout_CoversConfiguredSenderTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.BrokerURLs = []string{"tcp://192.0.2.1:1883"}
	cfg.Session.ClientID = "packet-budget-sender"
	cfg.Sender.Timeout = 5 * time.Minute

	f := &Factory{}
	session, err := f.NewSession(context.Background(), ports.SessionSpec{ID: "s1", Config: &cfg})
	require.NoError(t, err)
	_, err = f.NewSender(context.Background(), ports.SenderSpec{ID: "tx", Config: &cfg}, session)
	require.NoError(t, err)

	require.GreaterOrEqual(t, session.(*Session).packetTimeout(), 5*time.Minute,
		"must not pre-empt the configured sender timeout")
}

// TestPacketTimeout_ShorterSenderDoesNotLowerTheBudget pins that a session
// shared by several senders keeps the longest of their budgets: the shortest
// one must not shorten the SDK's packet deadline for the others.
func TestPacketTimeout_ShorterSenderDoesNotLowerTheBudget(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "packet-budget-shared",
	}, connectivity.SessionEphemeral, nil)

	t.Cleanup(func() { s.Router().shutdown() })

	NewSender(s, SenderOptions{Timeout: 5 * time.Minute})
	NewSender(s, SenderOptions{Timeout: time.Second})

	require.GreaterOrEqual(t, s.packetTimeout(), 5*time.Minute)
}

// TestPacketTimeout_NeverBelowTheSDKDefault pins the floor: a session built
// with no timings at all still gets at least the default sender budget, so an
// acknowledgement the bridge is willing to wait 30 seconds for is not refused
// at the SDK's own 10.
func TestPacketTimeout_NeverBelowTheSDKDefault(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "packet-budget-bare",
	}, connectivity.SessionEphemeral, nil)

	require.GreaterOrEqual(t, s.packetTimeout(), DefaultSenderOptions().Timeout)
}
