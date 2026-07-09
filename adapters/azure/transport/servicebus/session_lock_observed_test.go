package servicebus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// sessionDeadlineMock is an asbAPI that ALSO exposes an observed
// session-lock deadline (sessionLockDeadliner), like the production
// sessionReceiverAdapter. lockedUntil is a function so the mock can track
// the fake clock.
type sessionDeadlineMock struct {
	*mockASBClient
	lockedUntil func() time.Time
}

func (m *sessionDeadlineMock) SessionLockedUntil() time.Time { return m.lockedUntil() }

var (
	_ asbAPI               = (*sessionDeadlineMock)(nil)
	_ sessionLockDeadliner = (*sessionDeadlineMock)(nil)
)

// --- c6-session-lock: renewal paces off the OBSERVED lock deadline ---------

// TestSessionRenewer_PacesFromObservedLockDeadline proves the session
// renewer schedules against the broker's ACTUAL lock deadline
// (SessionLockedUntil), not the configured lock_duration. The observed
// lock expires 4s out (→ 2s renewal interval) while the configured
// lock_duration is 30s (→ 15s fallback interval). Advancing the fake clock
// by only 2s fires a renewal iff the renewer honoured the observed
// deadline. It also asserts a SINGLE ticker is used (not a per-tick timer),
// preserving the Ticker-based renewer contract.
//
// Mutation: if sessionRenewInterval returned the fallback (15s) instead of
// remaining/2 (2s), advancing 2s would fire nothing and the renewal wait
// would time out.
func TestSessionRenewer_PacesFromObservedLockDeadline(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	var renews atomic.Int32
	inner := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renews.Add(1)
			return nil
		},
	}
	client := &sessionDeadlineMock{
		mockASBClient: inner,
		// Observed broker lock expires 4s ahead of NOW → 2s renewal cadence.
		lockedUntil: func() time.Time { return fake.Now().Add(4 * time.Second) },
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:    "q",
		SessionID:    "s1",
		LockDuration: 30 * time.Second, // configured/2 = 15s fallback
		Client:       client,
		Clock:        fake,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, recv.ensureClient(context.Background()))

	renewCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go recv.runSessionRenewer(renewCtx, recv.cfg.LockDuration/2) // fallback 15s

	waitUntil(t, 2*time.Second, func() bool { return fake.TickerCount() >= 1 },
		"renewer ticker never registered")
	require.Equal(t, 1, fake.TickerCount(),
		"renewer must use a SINGLE ticker (not a per-tick timer)")

	fake.Advance(2 * time.Second) // observed cadence; the 15s fallback would NOT fire
	waitUntil(t, 2*time.Second, func() bool { return renews.Load() >= 1 },
		"session renewal must pace off the OBSERVED lock deadline (2s), not the configured lock_duration/2 (15s)")

	cancel()
}

// TestSessionRenewInterval_PrefersObservedOverConfigured is the unit-level
// companion: it pins sessionRenewInterval to half the remaining observed
// lock, and its floor at MinAutoExtendInterval.
func TestSessionRenewInterval_PrefersObservedOverConfigured(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	client := &sessionDeadlineMock{
		mockASBClient: &mockASBClient{},
		lockedUntil:   func() time.Time { return fake.Now().Add(6 * time.Second) },
	}
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:    "q",
		SessionID:    "s1",
		LockDuration: 30 * time.Second,
		Client:       client,
		Clock:        fake,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, recv.ensureClient(context.Background()))

	// remaining 6s → 3s, well under the 15s configured/2 fallback.
	require.Equal(t, 3*time.Second, recv.sessionRenewInterval(15*time.Second))

	// A near-expiry observed lock (remaining 1s → 500ms) floors at
	// MinAutoExtendInterval (default 1s) so the ticker interval is always
	// strictly positive.
	client.lockedUntil = func() time.Time { return fake.Now().Add(time.Second) }
	require.Equal(t, time.Second, recv.sessionRenewInterval(15*time.Second))
}

// TestSessionRenewInterval_LapsedLockReArmsAtFloor covers the fix #4
// remaining<=0 branch: when the OBSERVED session lock has already lapsed,
// the renewer must re-arm at the FLOOR (ASAP) so it attempts to reclaim the
// session immediately — NOT at the wider configured fallback (up to
// lock_duration/2), which would leave an already-expired lock un-renewed
// for seconds while another consumer could pick up the session.
//
// Mutation: fall the remaining<=0 branch through to `fallback` (15s)
// instead of the floor. Then both a just-expired and a long-expired lock
// return 15s and the floor assertions FAIL.
func TestSessionRenewInterval_LapsedLockReArmsAtFloor(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	client := &sessionDeadlineMock{
		mockASBClient: &mockASBClient{},
		// Observed lock deadline is NOW → remaining == 0 (lapsed).
		lockedUntil: func() time.Time { return fake.Now() },
	}
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:    "q",
		SessionID:    "s1",
		LockDuration: 30 * time.Second, // configured/2 = 15s fallback
		Client:       client,
		Clock:        fake,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, recv.ensureClient(context.Background()))

	const fallback = 15 * time.Second
	floor := time.Second // default MinAutoExtendInterval

	// remaining == 0 → floor, not the 15s fallback.
	require.Equal(t, floor, recv.sessionRenewInterval(fallback),
		"a lapsed session lock must re-arm at the floor (ASAP), not the configured fallback")

	// A long-expired deadline (remaining < 0) must also floor, not fall back.
	client.lockedUntil = func() time.Time { return fake.Now().Add(-10 * time.Second) }
	require.Equal(t, floor, recv.sessionRenewInterval(fallback),
		"a long-expired session lock must still re-arm at the floor")
}
