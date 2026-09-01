package bootstrap

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// closeFailSession is a session whose disconnect fails, so the runtime holding
// it reports an error from Stop.
type closeFailSession struct {
	events chan ports.SessionEvent
}

func (s *closeFailSession) Start(context.Context) error { return nil }
func (s *closeFailSession) Reconcile(context.Context, connectivity.SessionPlan) error {
	return nil
}

func (s *closeFailSession) Health(context.Context) ports.SessionHealth {
	return ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}
}
func (s *closeFailSession) Events() <-chan ports.SessionEvent { return s.events }
func (s *closeFailSession) Close(context.Context) error       { return errors.New("broker close hung") }

// closableStore is a lease store that records whether its handle was released.
type closableStore struct {
	ports.LeaseStore
	closed atomic.Int32
}

func (s *closableStore) Close() error { s.closed.Add(1); return nil }

type closableStoreFactory struct{ store *closableStore }

func (f *closableStoreFactory) NewLeaseStore(context.Context, ports.PluginConfig) (ports.LeaseStore, error) {
	return f.store, nil
}

func (f *closableStoreFactory) NewOutboxStore(
	context.Context, ports.PluginConfig, ports.OutboxRuntimeOptions,
) (ports.OutboxStore, error) {
	return nil, errors.New("no outbox store in this profile")
}

func (f *closableStoreFactory) NewDLQStore(context.Context, ports.PluginConfig) (ports.DLQStore, error) {
	return nil, errors.New("no dlq store in this profile")
}

// TestAppApplyPrepareCommit_OldStopFailure_ReleasesPlanAndWedges: prepare/commit
// stops the old runtime before committing the replacement, and a failed stop
// used to return with the old runtime still installed. That runtime can never
// serve again — Runtime.Stop has no early error return, so it has already
// cancelled its work context and closed everything it owned — so the process
// bridged nothing behind a green /live. Worse, appliedRef and the applied
// fingerprint still named its config, so when the admin transaction rolled the
// file back and the watcher re-emitted that same config, applyLogicalIfChanged
// fingerprint-matched and SKIPPED the rebuild: nothing ever recovered.
//
// The prepared (never-committed) plan's store handles leaked on the same path.
func TestAppApplyPrepareCommit_OldStopFailure_ReleasesPlanAndWedges(t *testing.T) {
	oldCfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "bridge-x", DrainTimeout: "200ms"},
	}

	// An old runtime whose Stop fails: the unmanaged session refuses to close, so
	// the stop inside the swap below reports an error.
	oldRuntime := goruntime.New(goruntime.WithInstanceID("old"))
	require.NoError(t, oldRuntime.AddRoute(
		goruntime.RouteConfig{ID: "r1"},
		nil, nil, &closeFailSession{events: make(chan ports.SessionEvent, 1)}, nil,
	))

	// A real prepared-but-never-committed plan, so the store handle it opened is
	// observable.
	store := &closableStore{}
	planCfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "bridge-x"},
		Stores: ports.StoresConfig{Lease: &ports.StoreConfig{Type: "closable"}},
	}
	buildPlan, err := bridge.NewBuilder(planCfg).
		RegisterStoreFactory("closable", &closableStoreFactory{store: store}).
		Plan(context.Background())
	require.NoError(t, err)

	app := NewApp(testBootstrapCfg())
	app.runtimeRef.Set(oldRuntime)
	app.appliedRef.Set(oldCfg)
	app.lastAppliedFingerprint = "old-fingerprint"

	plan := &runtimePlan{
		logical:  planCfg,
		inputs:   &resolvedInputs{},
		mode:     swapModePrepareCommit,
		registry: &factoryRegistry{cfg: planCfg},
		plan:     buildPlan,
	}

	err = app.applyPrepareCommit(context.Background(), plan, oldRuntime, oldCfg, nil)
	require.Error(t, err, "a failed old-runtime stop must abort the prepare/commit swap")

	assert.Nil(t, app.CurrentRuntime(),
		"a torn-down runtime must not stay installed as the current one")
	assert.True(t, app.runtimeTerminal(),
		"the App must wedge so /live fails closed and the backstop restarts the task")
	assert.Empty(t, app.lastAppliedFingerprint,
		"the applied fingerprint must be cleared so the rollback re-emit rebuilds instead of being skipped as already-applied")
	assert.Equal(t, int32(1), store.closed.Load(),
		"the never-committed plan's store handles must be released, not leaked")
}
