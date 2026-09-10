package bridge

// Residual test coverage for the builder-side production-readiness findings:
//
//   - Finding 4:  a shared_outbox route whose PRIMARY session resolves to nil
//     (stateless transport) must be rejected at build time — otherwise the
//     source is ACKed after the outbox persist but no drainer ever exists for
//     the partition (silent message loss).
//   - Finding 6:  an unreferenced SessionDef must not be constructed — nothing
//     would ever manage or close it, so it would leak one connection per
//     hot-reload swap.
//   - Finding 10: a receiver/sender/binding that references a DECLARED session
//     on a stateless transport must get a dedicated diagnosis, not the
//     misleading "references unknown session".

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// statelessSessionFn makes a countingTransportFactory behave like a stateless
// transport: NewSession returns (nil, nil), exactly as SQS/HTTP-style
// transports that need no session object do.
func statelessSessionFn(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	return nil, nil
}

// finding4Config declares a shared_outbox route whose primary session lives on
// the "stateless" transport, so the session resolves to nil at build time.
func finding4Config() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b-f4"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions:  []ports.SessionDef{{ID: "s-stateless", Transport: "stateless"}},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "fake"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "stateless"}},
		Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx", Address: "queue://out"}},
		Routes: []ports.RouteDef{{
			ID:           "r-f4",
			ReceiverID:   "rx",
			DeliveryMode: "shared_outbox",
			Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			Bindings:     []string{"b1"},
			Session:      &ports.RouteSessionDef{SessionID: "s-stateless", SenderID: "tx"},
		}},
	}
}

// TestBuilder_SharedOutboxRoute_StatelessPrimarySession_Rejected validates
// Finding 4: the build must fail hard when a shared_outbox route's primary
// session resolves to nil, instead of building a runtime that persists outbox
// records into a partition no drainer will ever poll.
func TestBuilder_SharedOutboxRoute_StatelessPrimarySession_Rejected(t *testing.T) {
	stateless := &countingTransportFactory{SessionFn: statelessSessionFn}
	b := NewBuilder(finding4Config()).
		RegisterTransportFactory("fake", &fakeTransportFactory{}).
		RegisterTransportFactory("stateless", stateless).
		RegisterStoreFactory("memory", &fakeStoreFactory{})

	_, err := b.Build(context.Background())
	require.Error(t, err, "shared_outbox with a nil primary session must fail the build (Finding 4)")
	assert.Contains(t, err.Error(), "shared_outbox")
	assert.Contains(t, err.Error(), "stateless")
	assert.Contains(t, err.Error(), `route "r-f4"`)
}

// TestBuilder_UnreferencedSession_NotConstructed validates Finding 6: a
// SessionDef that no route, binding, receiver, or sender references must not
// be constructed at all — an unreferenced session gets no manager and is never
// handed to the runtime, so building it would leak its connection on every
// reconfiguration swap.
func TestBuilder_UnreferencedSession_NotConstructed(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b-f6"},
		Sessions: []ports.SessionDef{
			{ID: "s-used", Transport: "counting"},
			{ID: "s-unused", Transport: "counting"},
		},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "counting", SessionID: "s-used"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "counting"}},
		Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx", Address: "topic/out"}},
		Routes: []ports.RouteDef{{
			ID:           "r-f6",
			ReceiverID:   "rx",
			DeliveryMode: "direct_hold",
			Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			Bindings:     []string{"b1"},
		}},
	}

	counting := &countingTransportFactory{}
	b := NewBuilder(cfg).RegisterTransportFactory("counting", counting)

	rt, err := b.Build(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = rt.Stop(stopCtx)
	})

	assert.Equal(t, 1, counting.SessionCalls,
		"only the referenced session must be constructed; an unreferenced one would leak (Finding 6)")
}

// TestBuilder_StatelessSessionReference_DedicatedError validates Finding 10:
// a receiver, sender, or shared_outbox binding that references a session which
// IS declared but resolves to nil (stateless transport) must produce a
// dedicated "stateless" diagnosis. The old code reported the misleading
// "references unknown session" for a session that was plainly declared in the
// blueprint.
func TestBuilder_StatelessSessionReference_DedicatedError(t *testing.T) {
	base := func() *ports.BridgeConfig {
		return &ports.BridgeConfig{
			Bridge:    ports.BridgeSettings{ID: "b-f10"},
			Sessions:  []ports.SessionDef{{ID: "s1", Transport: "stateless"}},
			Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "fake"}},
			Senders:   []ports.SenderDef{{ID: "tx", Transport: "fake"}},
			Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx", Address: "topic/out"}},
			Routes: []ports.RouteDef{{
				ID:           "r-f10",
				ReceiverID:   "rx",
				DeliveryMode: "direct_hold",
				Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
				Bindings:     []string{"b1"},
			}},
		}
	}

	build := func(cfg *ports.BridgeConfig) error {
		b := NewBuilder(cfg).
			RegisterTransportFactory("fake", &fakeTransportFactory{}).
			RegisterTransportFactory("stateless", &countingTransportFactory{SessionFn: statelessSessionFn})
		_, err := b.Build(context.Background())
		return err
	}

	t.Run("receiver names a declared stateless session", func(t *testing.T) {
		cfg := base()
		cfg.Receivers[0].SessionID = "s1"
		err := build(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stateless", "declared-but-nil session needs the dedicated diagnosis")
		assert.NotContains(t, err.Error(), "unknown session")
	})

	t.Run("sender names a declared stateless session", func(t *testing.T) {
		cfg := base()
		cfg.Senders[0].SessionID = "s1"
		err := build(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stateless")
		assert.NotContains(t, err.Error(), "unknown session")
	})

	t.Run("binding names a declared stateless session", func(t *testing.T) {
		cfg := base()
		cfg.Bindings[0].SessionID = "s1"
		err := build(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stateless")
		assert.NotContains(t, err.Error(), "unknown session")
	})

	t.Run("undeclared session still reports unknown", func(t *testing.T) {
		cfg := base()
		cfg.Receivers[0].SessionID = "ghost"
		err := build(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `unknown session "ghost"`)
	})
}
