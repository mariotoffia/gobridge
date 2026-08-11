package dlq_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// ═══════════════════════════════════════════════════════════════════════════
// DLQ writes are synchronous and confirmed.
//
// Route returns only once the entry is durably written (nil) or has
// permanently failed (non-nil). Callers must not settle the source delivery
// or outbox record on a non-nil return, keeping failure evidence at least as
// durable as the message it describes.
// ═══════════════════════════════════════════════════════════════════════════

func syncEnv(id string) *messaging.Envelope {
	return messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      id,
		Subject: "test/dlq-sync",
		Payload: []byte("payload"),
	})
}

// TestRouter_Route_ConfirmedBeforeReturn proves the core guarantee: the
// entry is present in the store the instant Route returns nil.
func TestRouter_Route_ConfirmedBeforeReturn(t *testing.T) {
	store := NewFakeStore()
	router := dlq.NewFromConfig(dlq.Config{Store: store})

	err := router.Route(
		context.Background(), syncEnv("confirmed-1"),
		"route-1", "bind-1", "", "sess-1", "src-1",
		shared.ErrUnavailable, 1,
	)

	require.NoError(t, err)
	assert.Equal(t, 1, store.Count(), "entry must be persisted before Route returns")
}

// TestRouter_Route_PropagatesWriteError verifies that a persistent store
// failure surfaces as a non-nil error so the caller leaves the source
// unsettled rather than silently dropping the evidence.
func TestRouter_Route_PropagatesWriteError(t *testing.T) {
	store := NewFakeStore()
	store.WriteErr = errors.New("store unavailable")
	router := dlq.NewFromConfig(dlq.Config{Store: store, WriteMaxAttempts: 1})

	err := router.Route(
		context.Background(), syncEnv("propagate-1"),
		"route-1", "bind-1", "", "sess-1", "src-1",
		shared.ErrUnavailable, 1,
	)

	require.Error(t, err)
	assert.Equal(t, 0, store.Count(), "no entry should be recorded on write failure")
}

// TestRouter_ConcurrentRoute exercises many concurrent Route calls. With the
// race detector enabled this guards against shared-state corruption now that
// writes are synchronous; every entry must land.
func TestRouter_ConcurrentRoute(t *testing.T) {
	store := NewFakeStore()
	router := dlq.NewFromConfig(dlq.Config{Store: store, WriteMaxAttempts: 1})

	var wg sync.WaitGroup
	const goroutines = 10
	const iterations = 50

	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range iterations {
				_ = router.Route(
					context.Background(), syncEnv("conc"),
					"route-1", "bind-1", "", "sess-1", "src-1",
					shared.ErrUnavailable, g*iterations+i,
				)
			}
		}(g)
	}
	wg.Wait()

	assert.Equal(t, goroutines*iterations, store.Count(),
		"every concurrent Route must durably record its entry")
}

// TestRouter_LeaseNotHeld_Refuses verifies that when a lease check is wired
// and the lease is absent, Route refuses with an error so the caller does not
// settle a message this instance no longer owns.
func TestRouter_LeaseNotHeld_Refuses(t *testing.T) {
	store := NewFakeStore()
	router := dlq.NewFromConfig(dlq.Config{Store: store, WriteMaxAttempts: 1})
	router.SetTokenFn(func(string) (persistence.LeaseToken, bool) { return persistence.LeaseToken{}, false })

	err := router.Route(
		context.Background(), syncEnv("lease-1"),
		"route-1", "bind-1", "", "sess-1", "src-1",
		shared.ErrUnavailable, 1,
	)

	require.Error(t, err)
	assert.Equal(t, 0, store.Count(), "no entry should be recorded without the lease")
}

// TestRouter_Route_AlreadyCanceledCtx_NoWrite verifies the first write attempt
// honors context cancellation before touching the store. The fake store
// ignores ctx, so without the pre-check an entry would be written even though
// shutdown has already cancelled the context; the pre-check makes Route return
// promptly so Run's wg.Wait is not delayed by a full WriteTimeout.
func TestRouter_Route_AlreadyCanceledCtx_NoWrite(t *testing.T) {
	store := NewFakeStore()
	router := dlq.NewFromConfig(dlq.Config{Store: store})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := router.Route(
		ctx, syncEnv("canceled-1"),
		"route-1", "bind-1", "", "sess-1", "src-1",
		shared.ErrUnavailable, 1,
	)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, store.Count(), "no write should occur once ctx is already canceled")
}
