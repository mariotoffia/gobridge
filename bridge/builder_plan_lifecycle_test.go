package bridge

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// highFourFailingConfig returns a config whose prepare() succeeds (stores open)
// but whose complete() fails: the direct_hold route defaults its failure
// handling to "dlq" while no DLQ store is configured, so ValidateRoutes rejects
// it at the end of complete — the same abandon-after-prepare shape Finding 2
// exercises, reused here for the one-shot invariant.
func highFourFailingConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "closable"},
			Outbox: &ports.StoreConfig{Type: "closable"},
		},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "fake"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "fake"}},
		Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx", Address: "queue://out"}},
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "rx", DeliveryMode: "direct_hold", Bindings: []string{"b1"}},
		},
	}
}

// highFourValidConfig returns a config whose prepare() AND complete() both
// succeed. It is used to exercise Close/Abort on a plan that is deliberately
// never committed: the prep-opened stores must still be released.
func highFourValidConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "closable"},
			Outbox: &ports.StoreConfig{Type: "closable"},
		},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "fake"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "fake"}},
		Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx", Address: "queue://out"}},
		Routes: []ports.RouteDef{
			{
				ID: "r1", ReceiverID: "rx", DeliveryMode: "direct_hold", Bindings: []string{"b1"},
				// drop policies keep the route valid without a DLQ store.
				Policy: ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			},
		},
	}
}

// TestBuildPlan_FailedCommitIsNotRetryable covers: a Commit is one-shot
// even when it FAILS. complete()'s failure defers close the prep-opened store
// handles, so a retried Commit would build a runtime over already-closed stores
// (and double-close them). The plan is marked consumed BEFORE complete runs, so
// a second Commit is rejected with "already committed" and the closed handles
// are never touched again.
func TestBuildPlan_FailedCommitIsNotRetryable(t *testing.T) {
	ctx := context.Background()
	outbox := &closableOutboxStore{}
	lease := &closableLeaseStore{}

	b := NewBuilder(highFourFailingConfig()).
		RegisterTransportFactory("fake", &fakeTransportFactory{}).
		RegisterStoreFactory("closable", &closableStoreFactory{lease: lease, outbox: outbox})

	plan, err := b.Plan(ctx)
	require.NoError(t, err)
	require.NotNil(t, plan)

	// First Commit fails in complete(); its defers close the prep stores once.
	_, err = plan.Commit(ctx)
	require.Error(t, err, "complete must reject a dlq-default route with no DLQ store")
	require.Equal(t, int32(1), outbox.closes.Load(), "failed complete closes the prep outbox once")

	// Second Commit must be REJECTED as one-shot, not re-run complete over the
	// now-closed stores (which would double-close them — the hazard).
	_, err2 := plan.Commit(ctx)
	require.Error(t, err2)
	assert.Contains(t, err2.Error(), "already committed",
		"a failed Commit must still consume the plan so a retry is rejected, not re-run over closed stores")
	assert.Equal(t, int32(1), outbox.closes.Load(),
		"a rejected retry must not touch (double-close) the already-released handles")
}

// TestBuildPlan_CloseReleasesUncommittedStores covers: a plan that is
// prepared but never committed leaks the SQLite/DynamoDB handles prepare opened
// unless Close/Abort releases them. Close is idempotent and, once a plan is
// closed, Commit is rejected.
func TestBuildPlan_CloseReleasesUncommittedStores(t *testing.T) {
	ctx := context.Background()
	outbox := &closableOutboxStore{}
	lease := &closableLeaseStore{}

	b := NewBuilder(highFourValidConfig()).
		RegisterTransportFactory("fake", &fakeTransportFactory{}).
		RegisterStoreFactory("closable", &closableStoreFactory{lease: lease, outbox: outbox})

	plan, err := b.Plan(ctx)
	require.NoError(t, err)
	require.NotNil(t, plan)
	require.Equal(t, int32(0), outbox.closes.Load(), "prepare must not close what it opened")

	plan.Close()
	require.Equal(t, int32(1), outbox.closes.Load(), "Close must release the prep-opened outbox handle")
	require.Equal(t, int32(1), lease.closes.Load(), "Close must release the prep-opened lease handle")

	// Idempotent: a second Close (or its Abort alias) must not double-close.
	plan.Abort()
	plan.Close()
	require.Equal(t, int32(1), outbox.closes.Load(), "Close/Abort must be idempotent")

	// A Commit after Close is rejected — the stores are gone.
	_, err = plan.Commit(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "after Close/Abort")
}

// TestBuildPlan_CloseAfterCommitIsNoOp covers's other half: once a plan
// is committed (successfully), Close must NOT close the store handles — the
// runtime now owns them and closes them on Stop. Close on a committed plan is a
// deliberate no-op so it can never double-close a live runtime's stores.
func TestBuildPlan_CloseAfterCommitIsNoOp(t *testing.T) {
	ctx := context.Background()
	outbox := &closableOutboxStore{}
	lease := &closableLeaseStore{}

	b := NewBuilder(highFourValidConfig()).
		RegisterTransportFactory("fake", &fakeTransportFactory{}).
		RegisterStoreFactory("closable", &closableStoreFactory{lease: lease, outbox: outbox})

	plan, err := b.Plan(ctx)
	require.NoError(t, err)

	rt, err := plan.Commit(ctx)
	require.NoError(t, err)
	require.NotNil(t, rt)

	plan.Close()
	assert.Equal(t, int32(0), outbox.closes.Load(),
		"Close on a committed plan must not close the runtime's stores")
}
