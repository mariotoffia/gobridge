package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

// The DEFINITIVE terminal contract of an in-band apply is ports.ErrApplyInFlight
// (relocated there so the httpapi module can errors.Is it across the module
// boundary). applyCommitted returns, wrapped with %w, one of:
//
//   - nil                                       → swap SUCCEEDED; runtime runs cfg. No rollback.
//   - errors.Is(err, ports.ErrApplyInFlight)    → committed but running state not
//     confirmed (bridge paused/deferred, or swap still in-flight past the apply
//     deadline). DO NOT roll back.
//   - any other err != nil                      → DEFINITIVE FAILURE (pre-submission
//     cancellation, or the swap was rejected and the Supervisor recovered the
//     OLD config). The runtime is NOT on cfg, so rolling back is SAFE.
//
// The Supervisor's onSwap callback is TOTAL — it fires for every apply path
// (success, failure, and the paused/deferred no-op) — so applyCommitted almost
// always resolves on a real terminal result and the apply deadline is only a
// backstop against a Supervisor that never reports at all.
//
// NOTE: the httpapi config-transaction layer must still be updated (tracked as a
// follow-up) to honour errors.Is(err, ports.ErrApplyInFlight) and NOT roll the
// durable config back in that case; until then it treats every non-nil result as
// a rollback-safe failure. The runtime-side contract here is in place regardless.

// defaultApplyDeadline bounds how long applyCommitted waits for the Supervisor
// to report an in-band swap's terminal result AFTER the config has been
// submitted. Because onSwap is total (it fires even for a paused/deferred apply
// and for every failure), a terminal result normally arrives well within this
// window; the deadline is a pure backstop against a Supervisor that never
// reports at all. It is deliberately larger than the Supervisor's own swap
// deadline (bridge.defaultSwapDeadline = 60s) so a merely-slow swap is not
// prematurely reported as in-flight. An operator-set drain_timeout can in
// principle push a real swap past this window; that no longer risks divergence
// (the total onSwap still delivers the terminal result, which the run loop
// forwards), it only costs the watcher one extra reconcile swap. On expiry the
// config is treated as committed-and-applying (ports.ErrApplyInFlight), never
// rolled back.
const defaultApplyDeadline = 90 * time.Second

// reloadPipeline bridges the httpapi ConfigApplier hook to the bridge
// Supervisor's asynchronous reload path so a config committed through the admin
// transactions API converges the running runtime IN-BAND, instead of leaving
// the committed_not_applied / errConfigApplyFailed path dead and relying solely
// on the file watcher to eventually pick up the change.
//
// The Supervisor applies configs from a single channel (drained by Supervisor.
// Run) and exposes no synchronous apply seam, so the pipeline owns that channel
// and:
//
//   - feeds an admin-committed config straight to the Supervisor, bypassing the
//     file-change debounce window, and BLOCKS the commit until the Supervisor
//     reports the swap result for that config (via the onSwap callback). A
//     failed swap is returned to httpapi, which surfaces committed_not_applied
//     so the operator reconciles rather than a false "committed";
//
//   - windows the file-watcher change stream exactly as the Supervisor used to
//     (the WindowedStrategy is moved out of the Supervisor, which now runs the
//     DirectStrategy), and DROPS the watcher's re-emit of a config the applier
//     just applied in-band. The commit's durable write changes the file's
//     content hash, so the watcher re-emits the same config; without this skip
//     every commit would cost a SECOND full stop→rebuild→start swap seconds
//     after the first — the double-rebuild lesson from the filebased fix.
//
// run is the single writer to changes(); applyCommitted and onSwap are called
// from other goroutines (the httpapi handler and the Supervisor's Run loop).
type reloadPipeline struct {
	registry *ports.Registry
	logger   *slog.Logger

	// notifier receives DEFINITIVE reconfiguration outcomes so the desired-config
	// manager can close its desired-vs-running divergence gap (XCUT-A / config
	// chunk-07 HIGH-1). It is a narrow interface (satisfied by *config.Manager's
	// NotifyApplyResult) so reload.go stays free of a config-module import and
	// tests can inject a fake. Nil disables the notification.
	notifier applyResultNotifier

	// out is the merged channel drained by Supervisor.Run. run is its sole
	// writer and closes it on shutdown.
	out chan *ports.BridgeConfig
	// admin carries committed configs from applyCommitted to run. handoff
	// resolves only after run either hands the config to the Supervisor or
	// observes cancellation; acceptance by this channel alone is not submission.
	admin chan adminApply

	mu sync.Mutex
	// lastAppliedFingerprint is the canonical content hash of the config the
	// applier last applied in-band, or "" when the config the runtime currently
	// runs was NOT set by an in-band commit. run clears it whenever it forwards
	// ANY config to the Supervisor (that config becomes what the runtime runs,
	// so a prior in-band fingerprint is now stale — skipping against it would
	// strand the runtime on an old config while disk holds a new one);
	// applyCommitted re-records it after a successful in-band apply. The file
	// watcher re-emits the committed config once after the commit's durable
	// write; run skips that single re-emit when its fingerprint matches, so a
	// commit costs exactly one runtime swap. Recorded only on a successful
	// apply, so a failed in-band apply is still retried by the watcher (the
	// historical safety net).
	lastAppliedFingerprint string
	// waiters resolves an applyCommitted call once the Supervisor reports the
	// swap outcome for its config. Keyed by config pointer identity: onSwap
	// receives the exact pointer the applier fed through (DirectStrategy and
	// run forward it unchanged), so correlation is unambiguous.
	waiters map[*ports.BridgeConfig]chan error

	// clk sources the apply deadline timer; tests inject a fake clock.
	clk clock.Clock
	// applyDeadline bounds the post-submission wait for a terminal swap result.
	applyDeadline time.Duration
}

// reloadOption configures a reloadPipeline. Kept unexported: the composition
// root uses the defaults and only tests tune the clock/deadline.
type reloadOption func(*reloadPipeline)

type adminApply struct {
	ctx     context.Context
	cfg     *ports.BridgeConfig
	handoff chan error
}

// applyResultNotifier receives DEFINITIVE reconfiguration outcomes so a
// desired-config manager can close its desired-vs-running divergence gap
// (config chunk-07 HIGH-1). *config.Manager satisfies it via NotifyApplyResult.
// Declared here as a narrow interface so the composition-root reload plumbing
// does not depend on the config module and tests can inject a fake.
type applyResultNotifier interface {
	NotifyApplyResult(cfg *ports.BridgeConfig, err error)
}

// withApplyResultNotifier wires the desired-config manager that receives
// definitive apply results (XCUT-A). Nil is ignored, leaving the pipeline
// notification-free (e.g. deployments with no divergence tracking).
func withApplyResultNotifier(n applyResultNotifier) reloadOption {
	return func(p *reloadPipeline) {
		if n != nil {
			p.notifier = n
		}
	}
}

// withApplyDeadline overrides the post-submission terminal-result wait.
func withApplyDeadline(d time.Duration) reloadOption {
	return func(p *reloadPipeline) {
		if d > 0 {
			p.applyDeadline = d
		}
	}
}

// withReloadClock injects the clock used for the apply deadline (tests only).
func withReloadClock(c clock.Clock) reloadOption {
	return func(p *reloadPipeline) {
		if c != nil {
			p.clk = c
		}
	}
}

func newReloadPipeline(registry *ports.Registry, logger *slog.Logger, opts ...reloadOption) *reloadPipeline {
	p := &reloadPipeline{
		registry:      registry,
		logger:        logger,
		out:           make(chan *ports.BridgeConfig),
		admin:         make(chan adminApply),
		waiters:       make(map[*ports.BridgeConfig]chan error),
		clk:           clock.System,
		applyDeadline: defaultApplyDeadline,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// changes returns the merged config channel for Supervisor.Run. The Supervisor
// must be constructed with bridge.NewDirectStrategy() so it applies each config
// immediately — file-change debouncing is done by run before it reaches here.
func (p *reloadPipeline) changes() <-chan *ports.BridgeConfig { return p.out }

// run merges debounced file-watcher changes and in-band admin commits onto the
// single channel the Supervisor consumes. It is the sole writer to p.out and
// closes it when ctx is cancelled. fileChanges is the ALREADY-debounced file
// stream (see main: bridge.NewWindowedStrategy(...).Filter).
func (p *reloadPipeline) run(ctx context.Context, fileChanges <-chan *ports.BridgeConfig) {
	defer close(p.out)
	// A committed config submitted through applyCommitted but not yet handed to
	// (and swapped by) the Supervisor when the pipeline stops would otherwise
	// leave its waiter blocked until the apply deadline. Resolve any such waiter
	// with a committed-not-applied (no-rollback) result: on shutdown the durable
	// config is already written, so the runtime converges on it after restart —
	// rolling back would lose the operator's committed change.
	defer p.failPendingWaiters()
	for {
		select {
		case <-ctx.Done():
			return
		case cfg, ok := <-fileChanges:
			if !ok {
				// Clean shutdown: the external WindowedStrategy closes its
				// output when ctx is cancelled, and that close can be selected
				// here instead of <-ctx.Done(). Return silently rather than
				// logging a spurious stream-closed error (mirrors supervisor.go,
				// which checks ctx before treating a close as a failure).
				if ctx.Err() != nil {
					return
				}
				// Genuine watcher failure (ctx still live): do NOT tear down a
				// healthy runtime (bridge Finding 1). Keep serving admin commits
				// and stop selecting on the dead channel.
				fileChanges = nil
				if p.logger != nil {
					p.logger.Error("config: file change stream closed; " +
						"live file reconfiguration disabled (admin commits still apply)")
				}
				continue
			}
			if p.isRedundantFileReload(cfg) {
				// The watcher re-emitted the config an admin commit just applied
				// in-band. The manager already recorded THIS pointer as
				// desiredConfig, so skipping the swap without acking would pin
				// ReconfigurePending true forever for a config the runtime
				// already runs. The runtime runs this exact content — ack it as
				// applied so desired-vs-running divergence clears (XCUT-A).
				if p.notifier != nil {
					p.notifier.NotifyApplyResult(cfg, nil)
				}
				if p.logger != nil {
					p.logger.Debug("config: file reload matches the config just applied in-band; " +
						"skipping redundant runtime swap")
				}
				continue
			}
			if !p.forward(ctx, cfg) {
				return
			}
		case apply := <-p.admin:
			if !p.forwardAdmin(ctx, apply) {
				return
			}
		}
	}
}

// forward hands cfg to the Supervisor, returning false when ctx is cancelled.
//
// Any config forwarded becomes the config the runtime runs, so it invalidates
// the previously recorded in-band fingerprint. Clear it BEFORE the send: the
// admin path re-establishes the fingerprint via recordApplied only AFTER the
// swap succeeds — which is strictly after this send completes (the Supervisor
// drains p.out, applies, then fires onSwap) — so the dedup of the single
// watcher re-emit is preserved. A file forward simply leaves it cleared (the
// watcher does its own change dedup via its lastHash), so a later re-emit is
// only ever skipped against the config the runtime actually runs.
func (p *reloadPipeline) forward(ctx context.Context, cfg *ports.BridgeConfig) bool {
	p.clearAppliedFingerprint()
	select {
	case p.out <- cfg:
		return true
	case <-ctx.Done():
		return false
	}
}

// forwardAdmin hands a committed config to the Supervisor. Cancellation of the
// request remains rollback-safe until the Supervisor receives the config. Once
// received, a nil handoff result makes applyCommitted wait for the terminal
// swap result independently of the request lifetime.
func (p *reloadPipeline) forwardAdmin(ctx context.Context, apply adminApply) bool {
	p.clearAppliedFingerprint()
	select {
	case p.out <- apply.cfg:
		apply.handoff <- nil
		return true
	case <-apply.ctx.Done():
		apply.handoff <- apply.ctx.Err()
		return true
	case <-ctx.Done():
		apply.handoff <- ports.ErrApplyInFlight
		return false
	}
}

// clearAppliedFingerprint drops any recorded in-band fingerprint so no watcher
// re-emit is skipped until the next successful in-band apply re-records one.
func (p *reloadPipeline) clearAppliedFingerprint() {
	p.mu.Lock()
	p.lastAppliedFingerprint = ""
	p.mu.Unlock()
}

// applyCommitted is the httpapi ConfigApplier hook. It converges the running
// runtime on cfg in-band by feeding it to the Supervisor (bypassing the debounce
// window) and returns a DEFINITIVE TERMINAL result (see ports.ErrApplyInFlight
// for the full contract):
//
//   - it aborts ONLY while the config has not yet reached the Supervisor. A
//     pre-submission ctx cancellation means the runtime is UNTOUCHED, so the
//     wrapped ctx.Err() is a rollback-SAFE failure;
//
//   - once cfg is submitted applyCommitted stops honouring the request ctx
//     (decoupled so a client disconnect cannot masquerade as a failed swap) and
//     waits for the terminal onSwap result under a per-config apply deadline.
//     onSwap is TOTAL — the Supervisor fires it for success, definitive failure,
//     AND the paused/deferred no-op — so a terminal result normally arrives well
//     within the deadline. A definitive swap error (runtime recovered the old
//     config) is rollback-safe; a paused/deferred or deadline-expiry result
//     resolves to ports.ErrApplyInFlight, which the caller MUST NOT roll back.
//
// A non-nil error surfaces as committed_not_applied; the durable write already
// happened, so the operator reconciles. The httpapi consumer must still be
// taught to distinguish errors.Is(err, ports.ErrApplyInFlight) (no rollback)
// from a definitive failure (rollback) — tracked as a follow-up; this hook
// exposes the unambiguous signal regardless.
func (p *reloadPipeline) applyCommitted(ctx context.Context, cfg *ports.BridgeConfig) error {
	done := make(chan error, 1)
	apply := adminApply{
		ctx:     ctx,
		cfg:     cfg,
		handoff: make(chan error, 1),
	}
	p.mu.Lock()
	p.waiters[cfg] = done
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiters, cfg)
		p.mu.Unlock()
	}()

	select {
	case p.admin <- apply:
		// Accepted by the pipeline. Submission is confirmed below only after the
		// Supervisor drains changes().
	case <-ctx.Done():
		// Pre-submission cancellation: cfg never reached the Supervisor, so the
		// runtime is untouched and rolling the durable config back is safe.
		return fmt.Errorf("apply committed config: %w", ctx.Err())
	}

	if err := <-apply.handoff; err != nil {
		return fmt.Errorf("apply committed config: %w", err)
	}
	// Submitted: cfg is now in-flight to the Supervisor.

	// cfg is in-flight. Wait for the terminal onSwap result under the apply
	// deadline ONLY — deliberately NOT the request ctx: the swap cannot be
	// cancelled, so bailing on ctx here would re-open the rolled-back-but-still-
	// running window. Because onSwap is total, a terminal result almost always
	// arrives well within applyDeadline (the deadline is only a backstop).
	select {
	case err := <-done:
		if err != nil {
			// Either a definitive failure (Supervisor recovered the old config
			// → rollback safe) or a committed-not-applied paused/deferred result
			// (wraps ports.ErrApplyInFlight → no rollback). The wrapping is
			// preserved so the caller's errors.Is decides; applyCommitted does
			// not need to disambiguate here.
			return fmt.Errorf("apply committed config: %w", err)
		}
		// Record the fingerprint AFTER a successful apply so the file watcher's
		// (debounced) re-emit of this same on-disk config is recognised and
		// skipped — no second rebuild — while a failed apply is left to be
		// retried by the watcher.
		p.recordApplied(cfg)
		return nil
	case <-p.clk.After(p.applyDeadline):
		// Backstop: onSwap did not report within the deadline. The swap is still
		// running and MAY yet adopt cfg. Return the committed-not-applied signal.
		// ponytail: we do not re-thread the eventual onSwap back to the caller
		// (it already returned), so the watcher's re-emit of this config costs
		// one extra swap — correctness over the double-rebuild optimisation in
		// this rare slow-swap case.
		if p.logger != nil {
			p.logger.Warn("config: in-band apply did not report a terminal swap "+
				"result within the apply deadline; treating as committed & applying "+
				"(NOT rolled back)", "apply_deadline", p.applyDeadline)
		}
		return fmt.Errorf("apply committed config: %w", ports.ErrApplyInFlight)
	}
}

// deferredApplyResult renders the waiter result for a deferred (committed-not-
// applied) swap. Every cause resolves to ports.ErrApplyInFlight — the applier
// must never roll back a deferral — but the message names the actual cause so an
// operator reading a commit response is not sent after a pause that does not
// exist.
func deferredApplyResult(reason bridge.DeferReason) error {
	if reason == bridge.DeferReasonRolloutPending {
		return fmt.Errorf("config proposed to the coordinated cluster rollout barrier; "+
			"it takes effect on this node when the barrier commits: %w", ports.ErrApplyInFlight)
	}
	return fmt.Errorf("bridge paused; config recorded for resume: %w", ports.ErrApplyInFlight)
}

// onSwap is the Supervisor's swap callback. It logs every swap (preserving the
// binary's reconfiguration logging) and resolves the applyCommitted waiter, if
// any, for the swapped config. It is TOTAL: the Supervisor fires it for success,
// failure, AND the paused/deferred no-op, so a waiter is always resolved.
func (p *reloadPipeline) onSwap(ev bridge.SwapEvent) {
	if p.logger != nil {
		switch {
		case ev.Deferred && ev.DeferReason == bridge.DeferReasonRolloutPending:
			p.logger.Info("reconfiguration deferred to the coordinated cluster rollout barrier; " +
				"the delta is proposed cluster-wide and takes effect on this node when the " +
				"barrier commits (no operator action required)")
		case ev.Deferred:
			p.logger.Info("reconfiguration deferred (bridge paused by admin); " +
				"config recorded and will take effect on resume")
		case ev.Error != nil:
			p.logger.Error("reconfiguration failed",
				"swap_mode", ev.SwapMode, "error", ev.Error, "duration", ev.Duration)
		default:
			p.logger.Info("reconfiguration applied",
				"swap_mode", ev.SwapMode, "duration", ev.Duration)
		}
	}

	p.mu.Lock()
	w := p.waiters[ev.NewConfig]
	p.mu.Unlock()

	// Report DEFINITIVE reconfiguration outcomes back to the desired-config
	// manager so it clears or raises desired-vs-running divergence (XCUT-A). A
	// DEFERRED event (bridge paused: committed-but-not-applied, Error == nil) is
	// NOT a definitive apply, so it is skipped — the manager keeps
	// ReconfigurePending until a real result lands. ev.NewConfig is the EXACT
	// pointer the manager emitted on its change channel (forwarded unchanged
	// through the reconfig strategy, run, and the Supervisor), so the manager's
	// pointer-identity correlation holds; an in-band admin swap carries a foreign
	// pointer that NotifyApplyResult safely ignores.
	if p.notifier != nil && !ev.Deferred {
		p.notifier.NotifyApplyResult(ev.NewConfig, ev.Error)
	}

	if w == nil {
		return
	}
	// A deferred (paused) apply is committed-but-not-applied, not a failure:
	// resolve the waiter with a no-rollback signal so the applier does not
	// mistake it for either a successful swap or a rollback-safe failure.
	res := ev.Error
	if ev.Deferred {
		res = deferredApplyResult(ev.DeferReason)
	}
	// done is buffered (cap 1). A non-blocking send keeps the Supervisor's Run
	// loop unblocked even if failPendingWaiters (pipeline shutdown) raced and
	// already resolved this waiter — whichever result lands first wins and the
	// applier reads exactly one.
	select {
	case w <- res:
	default:
	}
}

// failPendingWaiters resolves every still-registered applyCommitted waiter with
// a committed-not-applied (no-rollback) result. Called when the pipeline stops:
// a config that was submitted but never got a terminal onSwap must not hang its
// waiter for the full apply deadline while the process shuts down.
func (p *reloadPipeline) failPendingWaiters() {
	p.mu.Lock()
	waiters := make([]chan error, 0, len(p.waiters))
	for _, w := range p.waiters {
		waiters = append(waiters, w)
	}
	p.mu.Unlock()
	for _, w := range waiters {
		select {
		case w <- fmt.Errorf("config pipeline stopped before the swap was confirmed: %w", ports.ErrApplyInFlight):
		default:
		}
	}
}

// recordApplied stores the canonical fingerprint of the just-applied committed
// config so run can skip the watcher's re-emit of it.
func (p *reloadPipeline) recordApplied(cfg *ports.BridgeConfig) {
	fp := p.canonicalFingerprint(cfg)
	if fp == "" {
		return
	}
	p.mu.Lock()
	p.lastAppliedFingerprint = fp
	p.mu.Unlock()
}

// isRedundantFileReload reports whether cfg (a config parsed from disk by the
// watcher) is byte-identical, in canonical wire form, to the config the applier
// last applied in-band.
func (p *reloadPipeline) isRedundantFileReload(cfg *ports.BridgeConfig) bool {
	fp := fingerprint(cfg)
	if fp == "" {
		return false
	}
	p.mu.Lock()
	last := p.lastAppliedFingerprint
	p.mu.Unlock()
	return fp == last
}

// canonicalFingerprint fingerprints cfg as the file watcher will observe it —
// after a parse round-trip. The applier holds the in-memory committed config;
// the watcher re-emits Parse(MarshalYAML(cfg)) from the identical on-disk
// projection (the config store writes MarshalYAML(cfg)). Canonicalising here so
// both sides fingerprint Parse(MarshalYAML(cfg)) makes the match exact without
// assuming a parse∘marshal fixed point. Returns "" when it cannot be computed
// (fails open: the config is applied, not skipped).
func (p *reloadPipeline) canonicalFingerprint(cfg *ports.BridgeConfig) string {
	canonical, err := reparse(cfg, p.registry)
	if err != nil || canonical == nil {
		return ""
	}
	return fingerprint(canonical)
}

// fingerprint returns a stable content hash of cfg in canonical wire form. Two
// configs with the same fingerprint marshal to identical bytes.
func fingerprint(cfg *ports.BridgeConfig) string {
	if cfg == nil {
		return ""
	}
	data, err := cfgparser.MarshalYAML(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// reparse projects cfg through the config store's wire form and back, matching
// exactly what the file watcher emits after the store persists a committed
// config (Parse(MarshalYAML(cfg))).
func reparse(cfg *ports.BridgeConfig, registry *ports.Registry) (*ports.BridgeConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	data, err := cfgparser.MarshalYAML(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return cfgparser.Parse(bytes.NewReader(data), cfgparser.FormatYAML, registry)
}
