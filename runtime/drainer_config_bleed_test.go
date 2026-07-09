package runtime_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// syncBuffer is a goroutine-safe bytes.Buffer for capturing slog output
// (Start's session goroutines may log concurrently with the test's read).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestStart_SharedSessionDrainer_RejectsConfigBleed covers audit R8 / Chunk 9
// HIGH: exactly one outbox drainer exists per session partition. When two
// DIFFERENT shared_outbox routes reference the same session with divergent
// sender/policy, the second route's records would silently drain under the first
// route's sender and replay/DLQ policy — a data-integrity hazard. Start must now
// REJECT this at construction (hard validation error) naming both routes, rather
// than warning and running with the bleed.
func TestStart_SharedSessionDrainer_RejectsConfigBleed(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	var logBuf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rt := newTestRuntime("bridge-drainer-bleed", outbox, lease, nil,
		goruntime.WithLogger(logger))

	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-shared-sess")

	// Two routes share one session but each wires its OWN sender instance — the
	// drainer for the partition can only use one, so the other's records would
	// take the wrong send path.
	for _, id := range []string{"route-first", "route-second"} {
		cfg := goruntime.RouteConfig{
			ID:     id,
			Policy: routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
			Bindings: []routing.DestinationBinding{
				{ID: id + "-binding", Address: "devices/" + id},
			},
		}
		sc := sessCfg
		if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), sess, &sc); err != nil {
			t.Fatalf("AddRoute(%s): %v", id, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := rt.Start(ctx)
	if err == nil {
		t.Fatalf("R8: Start must reject a shared-session drainer config bleed, got nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "route-first") || !strings.Contains(msg, "route-second") {
		t.Fatalf("R8: error must name both the drainer-owning and the bleeding route, got: %v", msg)
	}
	if !strings.Contains(msg, "mqtt-shared-sess") {
		t.Fatalf("R8: error must name the shared session partition, got: %v", msg)
	}
	// A rejected Start must not have spawned any background work needing teardown,
	// but Stop must remain safe to call.
	if serr := rt.Stop(context.Background()); serr != nil {
		t.Fatalf("Stop after rejected Start: %v", serr)
	}
}

// TestStart_SharedSessionDrainer_AllowsAlignedConfig proves the reject is
// SURGICAL: two routes that share a session AND agree on sender + policy are the
// benign case (one correct drainer serves both), so Start must succeed. This
// guards against the hard error over-triggering on legitimate topologies.
func TestStart_SharedSessionDrainer_AllowsAlignedConfig(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	rt := newTestRuntime("bridge-drainer-aligned", outbox, lease, nil)

	sess := NewFakeSession()
	sender := NewFakeSender() // SAME sender instance for both routes
	sessCfg := fastSessionConfig("mqtt-aligned-sess")

	for _, id := range []string{"route-a", "route-b"} {
		cfg := goruntime.RouteConfig{
			ID:     id,
			Policy: routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
			Bindings: []routing.DestinationBinding{
				{ID: id + "-binding", Address: "devices/" + id},
			},
		}
		sc := sessCfg
		if err := rt.AddRoute(cfg, NewFakeReceiver(), sender, sess, &sc); err != nil {
			t.Fatalf("AddRoute(%s): %v", id, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("aligned shared-session routes must start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	waitFor(t, 2*time.Second, "sess started", sess.IsStarted)
}

// TestStart_SharedSessionDrainer_NormalizesDrainDefaults covers finding #12
// (MEDIUM): the drainer fingerprint must be computed over the NORMALIZED (post
// outbox.New defaulting) drain config, not the raw session fields. outbox.New
// defaults a zero DrainBatchSize/DrainMaxBatchSize/DrainMaxConcurrency/DrainTimeout
// to 100/500/10/10s, so two routes that share a session and are EFFECTIVELY
// identical — one leaving those fields zero, the other spelling out the exact
// defaults — build the same drainer. Comparing the RAW fields would see 0 vs 100
// and falsely reject a valid Start. With normalization the fingerprints are equal
// and Start succeeds.
func TestStart_SharedSessionDrainer_NormalizesDrainDefaults(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	rt := newTestRuntime("bridge-drainer-normalize", outbox, lease, nil)

	sess := NewFakeSession()
	sender := NewFakeSender() // SAME sender: the only difference is zero-vs-explicit drain config.

	base := fastSessionConfig("mqtt-normalize-sess")

	// route-zero leaves the tuning fields at zero (outbox.New will default them).
	zeroCfg := base
	zeroCfg.DrainBatchSize = 0
	zeroCfg.DrainMaxBatchSize = 0
	zeroCfg.DrainMaxConcurrency = 0
	zeroCfg.DrainTimeout = 0

	// route-explicit spells out exactly what outbox.New's defaulting produces.
	explicitCfg := base
	explicitCfg.DrainBatchSize = 100
	explicitCfg.DrainMaxBatchSize = 500
	explicitCfg.DrainMaxConcurrency = 10
	explicitCfg.DrainTimeout = 10 * time.Second

	specs := []struct {
		id string
		sc session.Config
	}{
		{"route-zero", zeroCfg},
		{"route-explicit", explicitCfg},
	}
	for _, spec := range specs {
		cfg := goruntime.RouteConfig{
			ID:     spec.id,
			Policy: routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
			Bindings: []routing.DestinationBinding{
				{ID: spec.id + "-binding", Address: "devices/" + spec.id},
			},
		}
		sc := spec.sc
		if err := rt.AddRoute(cfg, NewFakeReceiver(), sender, sess, &sc); err != nil {
			t.Fatalf("AddRoute(%s): %v", spec.id, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("finding #12: zero-vs-explicit-default drain configs must fingerprint EQUAL and start, got: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	waitFor(t, 2*time.Second, "sess started", sess.IsStarted)
}

// TestStart_SharedSessionDrainer_RejectsPolicyBleed_SameSender covers finding #14
// case (a): the reject guard must key on the drain/replay/DLQ POLICY fingerprint,
// not merely on sender identity. Two routes share a session and use the SAME
// sender instance, but diverge on OnPermanentFailure (DLQ vs drop) — a field the
// single per-partition drainer bakes in from whichever route it is built from, so
// the second route's poisoned records would be routed under the wrong terminal
// policy. OnPermanentFailure is deliberately chosen because it is UNIQUE to
// checkSharedOutboxDrainerConflicts' fingerprint: the earlier
// validateSharedOutboxPartitions guard (validator.go) only compares
// SendTimeout/MaxReplayAttempts/OnExpired, so those are held identical here to
// prove THIS guard — not the validator — rejects, even with identical senders.
func TestStart_SharedSessionDrainer_RejectsPolicyBleed_SameSender(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	rt := newTestRuntime("bridge-drainer-policybleed", outbox, lease, nil)

	sess := NewFakeSession()
	sender := NewFakeSender() // SAME sender instance for both routes.
	sessCfg := fastSessionConfig("mqtt-policybleed-sess")

	specs := []struct {
		id      string
		onPermF routing.FailureAction
	}{
		{"route-first", routing.FailureDLQ},
		{"route-second", routing.FailureDrop},
	}
	for _, spec := range specs {
		cfg := goruntime.RouteConfig{
			ID: spec.id,
			Policy: routing.RoutePolicy{
				DeliveryMode:       routing.DeliverySharedOutbox,
				OnPermanentFailure: spec.onPermF,
			},
			Bindings: []routing.DestinationBinding{
				{ID: spec.id + "-binding", Address: "devices/" + spec.id},
			},
		}
		sc := sessCfg
		if err := rt.AddRoute(cfg, NewFakeReceiver(), sender, sess, &sc); err != nil {
			t.Fatalf("AddRoute(%s): %v", spec.id, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := rt.Start(ctx)
	if err == nil {
		t.Fatal("finding #14a: divergent OnPermanentFailure on one session must reject even with the SAME sender")
	}
	msg := err.Error()
	// Assert THIS guard fired (not the sibling validator), so the test regresses
	// if checkSharedOutboxDrainerConflicts' fingerprint stops catching it.
	if !strings.Contains(msg, "resolve to divergent sender or drain/replay/DLQ policy") {
		t.Fatalf("finding #14a: expected the shared-drainer fingerprint guard to reject, got: %v", msg)
	}
	if !strings.Contains(msg, "route-first") || !strings.Contains(msg, "route-second") {
		t.Fatalf("error must name both routes, got: %v", msg)
	}
	if !strings.Contains(msg, "mqtt-policybleed-sess") {
		t.Fatalf("error must name the shared session partition, got: %v", msg)
	}
	if serr := rt.Stop(context.Background()); serr != nil {
		t.Fatalf("Stop after rejected Start: %v", serr)
	}
}

// TestStart_FanOutDrainer_RejectsPolicyBleed covers finding #14 case (b): the
// SECOND drainer-construction site — fan-out target sessions referenced by
// bindings and registered via RegisterSessionSender. Two routes fan out to the
// SAME registered session (same sender + drain config) but carry divergent
// OnPermanentFailure. Start builds exactly one drainer for that fan-out partition
// from whichever route claims it first, so the other route's records would drain
// under the wrong terminal policy. As in case (a), OnPermanentFailure is chosen
// so the sibling validator (which only compares SendTimeout/MaxReplayAttempts/
// OnExpired) passes and THIS guard's site-2 path is what rejects.
func TestStart_FanOutDrainer_RejectsPolicyBleed(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	rt := newTestRuntime("bridge-fanout-policybleed", outbox, lease, nil)

	// One shared fan-out target: single session, single sender, single config.
	fanSess := NewFakeSession()
	fanSender := NewFakeSender()
	fanCfg := fastSessionConfig("mqtt-fanout-shared")
	if err := rt.RegisterSessionSender(fanCfg, fanSess, fanSender); err != nil {
		t.Fatalf("RegisterSessionSender: %v", err)
	}

	// Two routes, each with its OWN distinct primary session (so site-1 does not
	// collide), both fanning out to the SAME registered session with DIVERGENT
	// route policy.
	specs := []struct {
		id      string
		primary string
		onPermF routing.FailureAction
	}{
		{"route-x", "mqtt-primary-x", routing.FailureDLQ},
		{"route-y", "mqtt-primary-y", routing.FailureDrop},
	}
	for _, spec := range specs {
		cfg := goruntime.RouteConfig{
			ID: spec.id,
			Policy: routing.RoutePolicy{
				DeliveryMode:       routing.DeliverySharedOutbox,
				OnPermanentFailure: spec.onPermF,
			},
			Bindings: []routing.DestinationBinding{
				{ID: spec.id + "-fanbind", SessionID: "mqtt-fanout-shared"},
			},
		}
		sc := fastSessionConfig(spec.primary)
		if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &sc); err != nil {
			t.Fatalf("AddRoute(%s): %v", spec.id, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := rt.Start(ctx)
	if err == nil {
		t.Fatal("finding #14b: divergent policy on a shared FAN-OUT (site-2) drainer must reject")
	}
	msg := err.Error()
	if !strings.Contains(msg, "resolve to divergent sender or drain/replay/DLQ policy") {
		t.Fatalf("finding #14b: expected the shared-drainer fingerprint guard (site-2) to reject, got: %v", msg)
	}
	if !strings.Contains(msg, "route-x") || !strings.Contains(msg, "route-y") {
		t.Fatalf("error must name both fan-out routes, got: %v", msg)
	}
	if !strings.Contains(msg, "mqtt-fanout-shared") {
		t.Fatalf("error must name the shared fan-out session partition, got: %v", msg)
	}
	if serr := rt.Stop(context.Background()); serr != nil {
		t.Fatalf("Stop after rejected Start: %v", serr)
	}
}

// TestSharedOutboxDrainer_ScaledTimeoutDefaultEquivalence_AllowsStart covers
// finding #24 (LOW): the drainer fingerprint must resolve the SCALED drain-timeout
// pair (PerRecordDrainTimeout/MaxDrainTimeout) the same way outbox.ComputeBatchDeadline
// does, instead of comparing them raw. In scaled mode a zero PerRecordDrainTimeout
// defaults to 3s and a zero MaxDrainTimeout to 10s at compute time, so two routes
// on one session partition that are effectively identical — route A leaves
// PerRecord zero, route B spells out 3s, both with Max=10s — have the SAME
// effective per-batch deadline and must NOT conflict.
//
// Mutation reasoning: with the PRE-FIX raw comparison the fingerprints differ
// (perRecordDrainTimeout 0 vs 3s) so checkSharedOutboxDrainerConflicts would
// reject Start with the "divergent sender or drain/replay/DLQ policy" error. With
// the fix normalizeScaledDrainTimeouts resolves both to (3s, 10s) → equal
// fingerprints → Start succeeds. (Fake clock only; no real sleeps — Start does no
// real-time waiting here.)
func TestSharedOutboxDrainer_ScaledTimeoutDefaultEquivalence_AllowsStart(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	rt := newTestRuntime("bridge-drainer-scaled-equiv", outbox, lease, nil)

	sess := NewFakeSession()
	sender := NewFakeSender() // SAME sender: the only difference is zero-vs-explicit scaled timeout.

	base := fastSessionConfig("mqtt-scaled-equiv-sess")

	// route-implicit: PerRecord left zero (scaled mode via non-zero Max) → resolves to 3s.
	implicitCfg := base
	implicitCfg.PerRecordDrainTimeout = 0
	implicitCfg.MaxDrainTimeout = 10 * time.Second

	// route-explicit: PerRecord spelled out as the compute-time default.
	explicitCfg := base
	explicitCfg.PerRecordDrainTimeout = 3 * time.Second
	explicitCfg.MaxDrainTimeout = 10 * time.Second

	specs := []struct {
		id string
		sc session.Config
	}{
		{"route-implicit", implicitCfg},
		{"route-explicit", explicitCfg},
	}
	for _, spec := range specs {
		cfg := goruntime.RouteConfig{
			ID:     spec.id,
			Policy: routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
			Bindings: []routing.DestinationBinding{
				{ID: spec.id + "-binding", Address: "devices/" + spec.id},
			},
		}
		sc := spec.sc
		if err := rt.AddRoute(cfg, NewFakeReceiver(), sender, sess, &sc); err != nil {
			t.Fatalf("AddRoute(%s): %v", spec.id, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("finding #24: scaled routes differing only zero-vs-default in the drain-timeout pair must fingerprint EQUAL and start, got: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	waitFor(t, 2*time.Second, "sess started", sess.IsStarted)
}
