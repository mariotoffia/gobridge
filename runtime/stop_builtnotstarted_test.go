package runtime_test

// Residual test coverage for the runtime side of production-readiness
// Findings 2 and 6 (contract): Runtime.Stop must release EVERY resource a
// build opened, even when the runtime was never Started (the supervisor stops
// a runtime whose swap failed) and even for sessions no session manager owns
// (non-shared_outbox binding sessions registered via RegisterSessionSender).

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// closableFakeLeaseStore is a lease store that records io.Closer.Close calls,
// so a test can prove Stop released the store handle.
type closableFakeLeaseStore struct {
	FakeLeaseStore
	closes int
}

func (s *closableFakeLeaseStore) Close() error {
	s.closes++
	return nil
}

// TestRuntime_Stop_BuiltNotStarted_ClosesSessionsAndStores validates
// (Finding 2): a runtime that was BUILT but never Started still owns opened
// sessions (route entry + registered session sender) and store handles. Stop
// on that runtime must close all of them — the supervisor relies on this to
// avoid leaking one full connection set per failed swap when a config flaps
// bad/good.
func TestRuntime_Stop_BuiltNotStarted_ClosesSessionsAndStores(t *testing.T) {
	lease := &closableFakeLeaseStore{}
	rt := goruntime.New(
		goruntime.WithInstanceID("c1-built-not-started"),
		goruntime.WithLeaseStore(lease),
	)

	cfg, recv, snd := helperMinimalRoute("c1-route")
	entrySess := NewFakeSession()
	entryCfg := session.Config{SessionID: "c1-entry"}
	require.NoError(t, rt.AddRoute(cfg, recv, snd, entrySess, &entryCfg))

	bindSess := NewFakeSession()
	require.NoError(t, rt.RegisterSessionSender(
		session.Config{SessionID: "c1-binding"}, bindSess, NewFakeSender()))

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, rt.Stop(stopCtx))

	assert.True(t, entrySess.IsClosed(),
		"route-entry session of a never-started runtime must be closed by Stop")
	assert.True(t, bindSess.IsClosed(),
		"session-sender session of a never-started runtime must be closed by Stop")
	assert.Equal(t, 1, lease.closes,
		"prep-opened store handle of a never-started runtime must be closed by Stop")
}

// TestRuntime_Stop_UnmanagedBindingSession_Closed validates the started-runtime
// half of Finding 6: a session registered via RegisterSessionSender for a
// non-shared_outbox route never gets a session manager (only shared_outbox
// drainer wiring creates one), so runtime.Stop's manager loop does not cover
// it. Stop must still close it, or every reconfiguration swap leaks one broker
// connection.
func TestRuntime_Stop_UnmanagedBindingSession_Closed(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("c1-unmanaged"))

	cfg, recv, snd := helperMinimalRoute("c1-direct-route")
	require.NoError(t, rt.AddRoute(cfg, recv, snd, nil, nil))

	unmanaged := NewFakeSession()
	require.NoError(t, rt.RegisterSessionSender(
		session.Config{SessionID: "c1-unmanaged-binding"}, unmanaged, NewFakeSender()))

	require.NoError(t, rt.Start(context.Background()))

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, rt.Stop(stopCtx))

	assert.True(t, unmanaged.IsClosed(),
		"a binding session without a session manager must be closed on Stop (Finding 6)")
}
