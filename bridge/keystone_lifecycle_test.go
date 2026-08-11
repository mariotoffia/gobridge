package bridge

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// ===========================================================================
// KEYSTONE lifecycle regression tests. These pin the exact seams the audit
// found: a healthy swap must not report Terminal, a shared exporter must
// survive a reload, a failed build after credential watchers start must not
// leak them, an abandoned build must release its sessions, and a hung swap
// build/complete phase must be bounded and recovered.
// ===========================================================================

// ---------------------------------------------------------------------------
// slowDrainTransportFactory — receivers that block ~delay on shutdown drain,
// so the OLD runtime's Stop holds the swap window open long enough to poll
// Supervisor.Terminal() throughout it.
// ---------------------------------------------------------------------------

type slowDrainReceiver struct{ delay time.Duration }

func (r *slowDrainReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	// Simulate an in-flight drain that keeps rt.Stop busy for the swap window.
	select {
	case <-time.After(r.delay):
	case <-time.After(2 * time.Second): // hard cap so a bug can't hang the test
	}
	return ctx.Err()
}

type slowDrainTransportFactory struct {
	fakeTransportFactory
	delay time.Duration
}

func (f *slowDrainTransportFactory) NewReceiver(_ context.Context, _ ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	return &slowDrainReceiver{delay: f.delay}, nil
}

var _ ports.TransportFactory = (*slowDrainTransportFactory)(nil)

// TestSupervisor_TerminalFalseDuringHealthySwap reproduces: while a
// perfectly healthy reconfiguration swap is in progress, Supervisor.Terminal()
// must stay false for the WHOLE window. Before the fix, the old (stopping)
// runtime reported terminal for the entire swap, so the liveness backstop
// killed the process mid-swap.
func TestSupervisor_TerminalFalseDuringHealthySwap(t *testing.T) {
	s := NewSupervisor(WithSupervisorBlueprintValidator(config.Validate))
	s.RegisterTransport("fake", &slowDrainTransportFactory{delay: 800 * time.Millisecond})
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan *ports.BridgeConfig, 1)
	errCh := runSupervisorAsync(ctx, s, supervisorTestConfig("r1"), changes)

	require.NotNil(t, waitForRuntime(s, 2*time.Second), "initial runtime must come up")

	// Continuously sample Terminal() from before the swap is triggered until the
	// swap has fully converged to r2. It must never be observed true.
	var sawTerminal atomic.Bool
	stopPoll := make(chan struct{})
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		for {
			select {
			case <-stopPoll:
				return
			default:
				if s.Terminal() {
					sawTerminal.Store(true)
				}
				time.Sleep(time.Millisecond) // OTHER: pace the bounded Terminal() sampling loop (exits on stopPoll)
			}
		}
	}()

	require.True(t, sendConfig(changes, supervisorTestConfig("r2"), time.Second), "swap config must enqueue")
	require.True(t, waitForRouteID(s, "r2", 5*time.Second), "swap must converge to r2")

	close(stopPoll)
	<-pollDone

	require.False(t, sawTerminal.Load(),
		"Supervisor.Terminal() must stay false during a healthy swap")

	cancel()
	<-errCh
}

// ---------------------------------------------------------------------------
// reloadExporter — counts Flush/Close so we can assert the shared exporter is
// flushed on every runtime Stop but NEVER closed by a runtime.
// ---------------------------------------------------------------------------

type reloadExporter struct {
	ports.NoopExporter
	flushCalls atomic.Int32
	closeCalls atomic.Int32
}

func (e *reloadExporter) Flush(context.Context) error {
	e.flushCalls.Add(1)
	return nil
}

func (e *reloadExporter) Close(context.Context) error {
	e.closeCalls.Add(1)
	return nil
}

var _ ports.MetricsExporter = (*reloadExporter)(nil)

// TestSupervisor_SharedExporterSurvivesReload reproduces: the
// Supervisor shares ONE metrics exporter across every runtime it builds. A
// runtime's Stop must FLUSH (buffered data must not be lost) but must NOT
// CLOSE the shared exporter — before the fix the first reload's Stop closed
// the shared exporter, permanently killing metrics for the process lifetime.
func TestSupervisor_SharedExporterSurvivesReload(t *testing.T) {
	exp := &reloadExporter{}

	s := NewSupervisor(WithSupervisorBlueprintValidator(config.Validate), WithSupervisorMetrics(exp))
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan *ports.BridgeConfig, 1)
	errCh := runSupervisorAsync(ctx, s, supervisorTestConfig("r1"), changes)

	require.NotNil(t, waitForRuntime(s, 2*time.Second), "initial runtime must come up")

	// Reload: the first runtime is stopped and a second built over the SAME
	// exporter.
	require.True(t, sendConfig(changes, supervisorTestConfig("r2"), time.Second))
	require.True(t, waitForRouteID(s, "r2", 5*time.Second), "reload must converge to r2")

	// The stopped first runtime must have FLUSHED but NOT CLOSED the exporter.
	require.GreaterOrEqual(t, int(exp.flushCalls.Load()), 1,
		"runtime.Stop must flush the shared exporter on reload")
	require.Equal(t, 0, int(exp.closeCalls.Load()),
		"runtime.Stop must NOT close the SHARED exporter")

	// Full process shutdown: the final runtime stops too. Still no runtime-driven
	// Close — the composition root is the sole owner of Close.
	cancel()
	<-errCh
	require.Equal(t, 0, int(exp.closeCalls.Load()),
		"no runtime may ever close the shared exporter; only the composition root does")
}

// ---------------------------------------------------------------------------
// Refresher-leak on ValidateRoutes failure (coupled HIGH). We build a config
// whose receiver carries a credentials_uri (so a credential-refresh watcher is
// started) but whose transport advertises NO capabilities, so the runtime's
// ValidateRoutes rejects the direct_hold route AFTER the watcher goroutine is
// running. The failure path must Close the refresher, cancelling the watch.
// ---------------------------------------------------------------------------

// recordingPushStore captures the context handed to Watch so the test can prove
// it is cancelled (i.e. the refresher was Closed) after the build fails.
type recordingPushStore struct {
	watched  atomic.Int32
	watchCtx atomic.Pointer[context.Context]
}

func (p *recordingPushStore) Watch(ctx context.Context, _ string) (<-chan *connectivity.CredentialSet, error) {
	p.watched.Add(1)
	c := ctx
	p.watchCtx.Store(&c)
	ch := make(chan *connectivity.CredentialSet)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

var _ ports.PushCredentialStore = (*recordingPushStore)(nil)

// credAwareReceiver implements ports.Receiver AND the bridge CredentialAware
// capability, so the refresher actually registers a watcher for it.
type credAwareReceiver struct{}

func (r *credAwareReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func (r *credAwareReceiver) ApplyCredentials(_ context.Context, _ *connectivity.CredentialSet) error {
	return nil
}

var _ CredentialAware = (*credAwareReceiver)(nil)

// noCapTransportFactory advertises NO capabilities, so a direct_hold route
// fails runtime ValidateRoutes (its source has neither visibility-extension nor
// http-endpoint). Its receiver is credential-aware.
type noCapTransportFactory struct {
	fakeTransportFactory
}

func (f *noCapTransportFactory) NewReceiver(_ context.Context, _ ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	return &credAwareReceiver{}, nil
}

func (f *noCapTransportFactory) Capabilities() []ports.Capability { return nil }

var _ ports.TransportFactory = (*noCapTransportFactory)(nil)

// credentialedReceiverConfig is a PluginConfig that also participates in
// credential resolution, so buildReceiversWithURIs records a receiver URI and
// the refresher starts a watcher for it.
type credentialedReceiverConfig struct {
	uri string
}

func (c *credentialedReceiverConfig) Kind() string                                       { return "credaware" }
func (c *credentialedReceiverConfig) Validate() error                                    { return nil }
func (c *credentialedReceiverConfig) CredentialsURI() string                             { return c.uri }
func (c *credentialedReceiverConfig) ApplyCredentials(*connectivity.CredentialSet) error { return nil }

var (
	_ ports.PluginConfig       = (*credentialedReceiverConfig)(nil)
	_ ports.CredentialedConfig = (*credentialedReceiverConfig)(nil)
)

func TestBuilder_RefresherClosedOnValidateRoutesFailure(t *testing.T) {
	const uri = "cred://receiver"

	pull := &fakeCredentialStore{creds: map[string]*connectivity.CredentialSet{
		uri: connectivity.NewCredentialSet(pwCred("u", "p"), nil),
	}}
	push := &recordingPushStore{}

	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "credaware"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "fake"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "queue://out"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx1",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"b1"},
				Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			},
		},
	}
	cfg.Receivers[0].SetDecoded(&credentialedReceiverConfig{uri: uri}, nil)

	b := NewBuilder(cfg,
		WithCredentialStore(pull),
		WithPushCredentialStore(push),
		WithLogger(slog.Default()),
	).
		RegisterTransportFactory("credaware", &noCapTransportFactory{}).
		RegisterTransportFactory("fake", &fakeTransportFactory{})

	rt, err := b.Build(context.Background())
	require.Error(t, err, "empty-capability source must fail direct_hold ValidateRoutes")
	require.Nil(t, rt)

	require.Equal(t, int32(1), push.watched.Load(),
		"the credential refresher must have started a watcher before validation failed")

	// The failure defer must have Closed the refresher, which cancels the ctx it
	// handed to push.Watch. Prove the leak is gone by observing that context is
	// Done (a leaked refresher would keep it live forever).
	watchCtx := push.watchCtx.Load()
	require.NotNil(t, watchCtx)
	require.Eventually(t, func() bool {
		select {
		case <-(*watchCtx).Done():
			return true
		default:
			return false
		}
	}, 2*time.Second, 5*time.Millisecond,
		"ValidateRoutes failure must Close the refresher, cancelling its watch (HIGH: refresher leak)")
}

// TestStopAbandoned_ReleasesBuiltRuntimeSessions covers the mechanism the
// initial-Start-failure fix relies on (coupled HIGH): a runtime that was BUILT
// but never Started must have its sessions released, not leaked. Supervisor.Run
// now calls stopAbandoned on an initial Start failure; a real Start failure
// after a successful build is not reachable through config alone (complete()
// already runs the same ValidateRoutes Start would), so we exercise the release
// path directly on a built-but-unstarted runtime.
func TestStopAbandoned_ReleasesBuiltRuntimeSessions(t *testing.T) {
	s := NewSupervisor(WithSupervisorBlueprintValidator(config.Validate))
	s.RegisterTransport("fake", &fakeTransportFactory{})
	tf := &trackingTransportFactory{failAt: -1}
	s.RegisterTransport("exclusive", tf)
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})

	ctx := context.Background()
	cfg := supervisorTestConfigWithSession("r1", "s1")

	rt, err := s.buildRuntime(ctx, cfg)
	require.NoError(t, err)
	require.NotNil(t, rt)

	sessions := tf.Sessions()
	require.Len(t, sessions, 1, "build must have opened exactly one session")
	require.Equal(t, 0, sessions[0].CloseCount(), "session must be open before cleanup")

	// This is exactly what Supervisor.Run does on an initial Start failure.
	s.stopAbandoned(ctx, rt, cfg)

	require.GreaterOrEqual(t, sessions[0].CloseCount(), 1,
		"stopAbandoned must release the built runtime's sessions (HIGH: initial Start leak)")
}

// TestSupervisor_HungSwapCompleteBoundedByDeadline covers the coupled HIGH: the
// prepare-commit swap's build/complete phase runs AFTER the old runtime is
// stopped, so a hung external construction (e.g. NewSession against a
// partitioned broker) would strand the bridge forever. The phase must be
// bounded by the swap deadline and route into recoverOldOrWedge, which resumes
// the old config.
func TestSupervisor_HungSwapCompleteBoundedByDeadline(t *testing.T) {
	s, ef := newTestSupervisorWithExclusive(WithSwapDeadline(150 * time.Millisecond))
	var hangNext atomic.Bool
	ef.SessionFn = func(ctx context.Context, spec ports.SessionSpec) (ports.Session, error) {
		if hangNext.CompareAndSwap(true, false) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &fakeSession{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan *ports.BridgeConfig, 1)
	errCh := runSupervisorAsync(ctx, s, supervisorTestConfigWithSession("r1", "s1"), changes)

	rt0 := waitForRuntime(s, 2*time.Second)
	require.NotNil(t, rt0, "initial runtime must come up")

	// Keep the durable partition identity unchanged so this test reaches the
	// intended lifecycle failure rather than the destructive-reload preflight.
	hangNext.Store(true)
	require.True(t, sendConfig(changes, supervisorTestConfigWithSession("r2", "s1"), time.Second))

	// The hung complete must be bounded by the swap deadline, fail the swap, and
	// recover the old config — installing a FRESH runtime (!= rt0) that is not
	// terminal.
	require.Eventually(t, func() bool {
		rt := s.Runtime()
		return rt != nil && rt != rt0
	}, 5*time.Second, 10*time.Millisecond,
		"a hung swap complete must be bounded and recovered to the old config")

	require.False(t, s.Terminal(), "recovery to the old config must not be terminal")
	rec := s.Runtime()
	require.NotNil(t, rec)
	routes := rec.Routes()
	require.Len(t, routes, 1)
	require.Equal(t, "r1", routes[0].ID, "recovered runtime must serve the old route")

	cancel()
	<-errCh
}

// TestSupervisor_StopBridgeThenStartBridge covers at the supervisor
// seam the http admin layer calls: a deliberate StopBridge must be a clean pause
// (runtime non-terminal, so /live stays 200 and the backstop does not restart),
// and a subsequent StartBridge must rebuild a fresh runtime and succeed — never
// a permanent single-use rejection.
func TestSupervisor_StopBridgeThenStartBridge(t *testing.T) {
	s := newTestSupervisor()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan *ports.BridgeConfig)
	errCh := runSupervisorAsync(ctx, s, supervisorTestConfig("r1"), changes)

	rt0 := waitForRuntime(s, 2*time.Second)
	require.NotNil(t, rt0)
	require.True(t, rt0.IsRunning())

	// Deliberate pause: clean stop, NOT terminal.
	require.NoError(t, s.StopBridge(ctx))
	require.False(t, rt0.IsRunning(), "StopBridge must stop the runtime")
	require.False(t, rt0.Terminal(), "a deliberate StopBridge must not be terminal")
	require.False(t, s.Terminal(), "supervisor must not report terminal after a clean pause")

	// Resume: builds a FRESH runtime and starts it (single-use runtime means
	// resume != in-place restart). Must succeed, not 409-forever.
	require.NoError(t, s.StartBridge(ctx), "StartBridge after StopBridge must succeed")

	rt1 := s.Runtime()
	require.NotNil(t, rt1)
	require.NotSame(t, rt0, rt1, "resume must build a fresh runtime")
	require.True(t, rt1.IsRunning(), "resumed runtime must be running")

	cancel()
	<-errCh
}

// TestSupervisor_StartBridge_RuntimeSurvivesRequestCtxCancel pins BLOCKER 1: the
// admin handler passes a request-scoped ctx (cancelled the instant it returns).
// Runtime.Start binds the runtime's lifetime to the ctx it is given and spawns a
// watcher that drives Stop when that ctx is cancelled, so StartBridge MUST start
// the resumed runtime under the long-lived Run ctx — not the request ctx — or
// /bridge/start returns 200 and the bridge self-stops milliseconds later.
func TestSupervisor_StartBridge_RuntimeSurvivesRequestCtxCancel(t *testing.T) {
	s := newTestSupervisor()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan *ports.BridgeConfig)
	errCh := runSupervisorAsync(ctx, s, supervisorTestConfig("r1"), changes)
	require.NotNil(t, waitForRuntime(s, 2*time.Second), "initial runtime must come up")

	require.NoError(t, s.StopBridge(ctx))

	// Mirror the admin handler exactly: a request-scoped ctx cancelled the moment
	// the handler returns.
	reqCtx, reqCancel := context.WithCancel(ctx)
	require.NoError(t, s.StartBridge(reqCtx), "StartBridge must succeed")
	reqCancel() // the HTTP handler returns and its defer cancel() fires

	rt1 := s.Runtime()
	require.NotNil(t, rt1)
	require.True(t, rt1.IsRunning(), "resumed runtime must be running")
	require.Never(t, func() bool { return !rt1.IsRunning() }, 300*time.Millisecond, 20*time.Millisecond,
		"resumed runtime must NOT self-stop when the admin request ctx is cancelled (BLOCKER 1)")

	cancel()
	<-errCh
}

// TestSupervisor_HungSwapAndHungRecovery_Wedges pins BLOCKER 2: when a swap
// fails because a broker is partitioned AND rebuilding the old config hits the
// same partition, the recovery build must be bounded by the swap deadline. An
// unbounded recovery build would keep apply() from returning, leaving
// swapping=true (and thus Terminal()==false) forever — a permanent silent outage
// with the liveness backstop disabled. Bounded, it wedges and reports terminal.
func TestSupervisor_HungSwapAndHungRecovery_Wedges(t *testing.T) {
	s, ef := newTestSupervisorWithExclusive(WithSwapDeadline(120 * time.Millisecond))
	// "s1" builds normally until failS1 is armed, after which both the swap and
	// recovery builds block. Keeping the session ID stable avoids changing the
	// durable partition identity under test.
	var failS1 atomic.Bool
	ef.SessionFn = func(ctx context.Context, spec ports.SessionSpec) (ports.Session, error) {
		if spec.ID == "s1" && failS1.Load() {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &fakeSession{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan *ports.BridgeConfig, 1)
	errCh := runSupervisorAsync(ctx, s, supervisorTestConfigWithSession("r1", "s1"), changes)
	require.NotNil(t, waitForRuntime(s, 2*time.Second), "initial runtime must come up")

	failS1.Store(true) // any later s1 build (the recovery) now hangs too
	require.True(t, sendConfig(changes, supervisorTestConfigWithSession("r2", "s1"), time.Second))

	require.Eventually(t, func() bool {
		return s.Terminal() && s.Runtime() == nil
	}, 3*time.Second, 10*time.Millisecond,
		"a hung swap AND hung recovery must wedge+terminal within the deadline (BLOCKER 2)")

	cancel()
	<-errCh
}

// TestSupervisor_PauseSurvivesConfigReload pins the maintenance-pause property:
// after a deliberate StopBridge, a config reload must be recorded but must NOT
// silently resume the bridge; a later StartBridge then resumes on the latest
// recorded config.
func TestSupervisor_PauseSurvivesConfigReload(t *testing.T) {
	s := newTestSupervisor()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan *ports.BridgeConfig, 1)
	errCh := runSupervisorAsync(ctx, s, supervisorTestConfig("r1"), changes)
	rt0 := waitForRuntime(s, 2*time.Second)
	require.NotNil(t, rt0)
	require.True(t, rt0.IsRunning())

	require.NoError(t, s.StopBridge(ctx), "deliberate maintenance pause")
	require.False(t, rt0.IsRunning())

	// A config reload arrives while paused. It is recorded but must not resume.
	require.True(t, sendConfig(changes, supervisorTestConfig("r2"), time.Second))
	require.Never(t, func() bool {
		rt := s.Runtime()
		return rt != nil && rt.IsRunning()
	}, 400*time.Millisecond, 20*time.Millisecond,
		"a config reload must not resume an admin-paused bridge")

	// Resume builds from the LATEST config recorded during the pause (r2).
	require.NoError(t, s.StartBridge(ctx))
	require.True(t, waitForRouteID(s, "r2", 2*time.Second),
		"resume must build the config recorded during the pause")

	cancel()
	<-errCh
}

// TestSupervisor_ConcurrentStartBridge_BuildsExactlyOnce pins the control-plane
// serialization (HIGH: TOCTOU): two concurrent resumes (an operator double-click
// or a client retry) must not each build a runtime. lifecycleMu serializes them
// so exactly one builds and the other observes a running runtime and no-ops;
// without it both build and one fully-started runtime (live consumers, held
// lease, open store handles) leaks with nothing to Stop it.
func TestSupervisor_ConcurrentStartBridge_BuildsExactlyOnce(t *testing.T) {
	s, ef := newTestSupervisorWithExclusive()
	var inBuild, maxConcurrent atomic.Int32
	ef.SessionFn = func(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
		n := inBuild.Add(1)
		for {
			m := maxConcurrent.Load()
			if n <= m || maxConcurrent.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond) // OTHER: race window — widen so an unserialized concurrent-build race overlaps
		inBuild.Add(-1)
		return &fakeSession{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan *ports.BridgeConfig)
	errCh := runSupervisorAsync(ctx, s, supervisorTestConfigWithSession("r1", "s1"), changes)
	require.NotNil(t, waitForRuntime(s, 2*time.Second))

	require.NoError(t, s.StopBridge(ctx)) // pause; runtime stopped, ready to resume
	maxConcurrent.Store(0)                // ignore the initial (sequential) build

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_ = s.StartBridge(ctx)
		}()
	}
	wg.Wait()

	require.Equal(t, int32(1), maxConcurrent.Load(),
		"concurrent StartBridge must not build two runtimes at once (lifecycleMu)")
	rt := s.Runtime()
	require.NotNil(t, rt)
	require.True(t, rt.IsRunning())

	cancel()
	<-errCh
}

func (c *credentialedReceiverConfig) FreezePluginConfig() ports.PluginConfig {
	frozen := *c
	return &frozen
}

var _ ports.FreezableConfig = (*credentialedReceiverConfig)(nil)
