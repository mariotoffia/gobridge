package config

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// MergeFunc combines an overlay config on top of a base config.
// The base must not be modified; a new ports.BridgeConfig should be returned.
type MergeFunc func(base, overlay *ports.BridgeConfig) (*ports.BridgeConfig, error)

// Layer represents a configuration source with optional change watching.
type Layer struct {
	Name    string
	Loader  ports.Loader
	Watcher ports.Watcher // nil if the source does not support watching
}

// watch (re)establishment backoff bounds. A layer whose watcher dies in
// steady state is re-established on this schedule; the runtime keeps serving
// the last good config throughout (never torn down for a watch failure).
const (
	watchRetryInitial = 500 * time.Millisecond
	watchRetryMax     = 30 * time.Second
)

// WatchStartError indicates a config layer's change watcher could not be
// established. It is returned from Watch when a watcher fails its FIRST
// (boot-time) establishment attempt: watching is a hard requirement at boot,
// so a composition root that cannot observe config changes must fail loudly
// (non-zero exit) rather than run blind (previously the failure was logged at
// Warn and the change channel closed, which drained and stopped the whole
// bridge with exit code 0 — a silent total outage). In STEADY state the same
// failure is NOT fatal — it is recorded (see Manager.WatchErrors) and retried
// with backoff while the current config keeps serving.
type WatchStartError struct {
	Layer string
	Err   error
}

func (e *WatchStartError) Error() string {
	return fmt.Sprintf("config manager: layer %q watcher failed to start: %v", e.Layer, e.Err)
}

func (e *WatchStartError) Unwrap() error { return e.Err }

// Manager coordinates multiple config sources in a layered stack.
// A base layer is loaded first, then overlays are merged on top in order.
// The merged result is validated before being returned.
//
// Manager satisfies ports.Loader, ports.Watcher, and ports.Reloader.
type Manager struct {
	base     Layer
	overlays []Layer
	mergeFn  MergeFunc
	logger   *slog.Logger
	clk      clock.Clock

	mu        sync.Mutex
	configs   map[string]*ports.BridgeConfig // cached per-layer configs
	watchErrs map[string]error               // layer name → current watch establishment error (degraded signal)
	// appliedVersion is the ports.BridgeConfig.Version of the last merged config
	// this instance successfully validated and emitted downstream (-1 before the
	// first emit). It is a per-INSTANCE convergence signal: because a rejected
	// layer update keeps the prior config locally (degraded state is local-only,
	// finding H-cluster-convergence), one node can sit on an older version while
	// another advances, and nothing here coordinates them. Surfacing the applied
	// version (AppliedVersion) plus logging every version change makes that
	// divergence OBSERVABLE so operators/monitoring can alert on cross-instance
	// version skew externally.
	//
	// NOTE: appliedVersion is the DESIRED version — the one this manager last
	// EMITTED downstream (asked the applier to run). It advances the moment a
	// merged config is validated and pushed, BEFORE any runtime swap has
	// actually succeeded. runningVersion below is the version the applier
	// CONFIRMED it applied. The two diverge after a failed runtime swap (the
	// supervisor recovers the previous runtime) — see NotifyApplyResult.
	appliedVersion int
	// runningVersion is the BridgeConfig.Version the downstream applier
	// (supervisor) last CONFIRMED it applied via NotifyApplyResult. It is the
	// truthful "what is actually serving traffic" signal, distinct from the
	// emitted/desired appliedVersion. On a failed swap the applier recovers the
	// previous runtime, so runningVersion stays behind the desired config and
	// ReconfigurePending reports the divergence — this stops the manager/source
	// from silently claiming a live reload took effect while the old runtime
	// still serves traffic (finding: desired-ahead-of-applied). -1 before the
	// first confirmed apply. NOTE: like appliedVersion this is operator-facing
	// observability only; the desired-vs-running DECISION is keyed on the content
	// fingerprints below, never on the non-unique Version.
	runningVersion int
	// lastApplyErr is the error from the most recent FAILED NotifyApplyResult for
	// the CURRENT desired config (nil once a later apply is confirmed, a newer
	// desired is emitted, or before any result is reported).
	lastApplyErr error
	// applyResultSeen becomes true after the first NotifyApplyResult that
	// pertains to the current desired config. Until an applier reports back,
	// ReconfigurePending stays false: an unwired composition root (no ack path)
	// must not look permanently out-of-sync just because nothing has confirmed
	// the boot config yet.
	applyResultSeen bool
	// desiredFingerprint / runningFingerprint correlate an emitted (desired)
	// config with the apply result reported for it by CONTENT, not by the
	// operator-controlled BridgeConfig.Version. Version is NOT unique: an
	// external writer can change config CONTENT while leaving the version field
	// unchanged (or two commits can reuse a number), so keying desired-vs-running
	// on Version HIDES a real divergence after a failed swap — the manager would
	// see appliedVersion == runningVersion and wrongly report "converged". These
	// SHA-256 fingerprints are taken over the REVEALED config (secrets included)
	// so any content change — including a secret-only edit — is detected. The
	// fingerprint ALSO covers each decoded PluginConfig's options: those live in
	// the `Config` fields tagged json:"-" on the blueprint, so a plain
	// json.Marshal of the config DROPS them and a change confined to a plugin's
	// options (or a plugin secret) at an unchanged Version would be INVISIBLE —
	// see configFingerprint, which projects each plugin's decoded options back in
	// (finding: plugin options omitted from the fingerprint). desiredFingerprint
	// is the last config EMITTED downstream; runningFingerprint is the last one the
	// applier CONFIRMED. ReconfigurePending is their inequality (finding:
	// desired/running keyed on non-unique Version).
	desiredFingerprint [sha256.Size]byte
	runningFingerprint [sha256.Size]byte
	// desiredConfig is the EXACT *ports.BridgeConfig pointer the manager last
	// emitted downstream. NotifyApplyResult correlates an apply result to the
	// desired config by this pointer identity: the applier echoes the exact config
	// it attempted back via SwapEvent.NewConfig (verified end to end — watchLoop →
	// WindowedStrategy → reload pipeline → Supervisor.onSwap all forward the
	// pointer without cloning or reparsing). Pointer identity, NOT the content
	// fingerprint, is what disambiguates an A→B→A flap: A#1 and A#2 are distinct
	// allocations with identical content, so a delayed ack for A#1 that lands after
	// A#2 was emitted is a DIFFERENT pointer and is ignored, whereas a content hash
	// (identical for A#1 and A#2) cannot tell them apart (finding: content-only
	// correlation lets a stale ack regress running).
	desiredConfig *ports.BridgeConfig
	// desiredHashErr is non-nil when the current desired config cannot be
	// fingerprinted (e.g. a NaN/Inf or otherwise unserialisable value reached via
	// the public ConditionDef.Value any). A config we cannot fingerprint can never
	// be PROVEN converged, so ReconfigurePending fails closed (reports pending)
	// instead of letting an unhashable config collapse to a zero fingerprint that
	// would falsely compare equal to another unhashable one and read as converged
	// (finding: marshal error collapses to a zero fingerprint).
	desiredHashErr error
	stopCh         chan struct{}
	doneCh         chan struct{} // closed when watchLoop exits
	running        bool
	// stopping guards stopCh's close so two concurrent Stop calls cannot both
	// close it (a double close panics). running is only cleared after doneCh, so
	// it cannot distinguish "already asked to stop" from "still running";
	// stopping does. Reset in Watch on (re)start.
	stopping bool
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithOverlay adds an overlay layer that is merged on top of the base
// (and any previously added overlays) in the order registered.
func WithOverlay(layer Layer) ManagerOption {
	return func(m *Manager) {
		m.overlays = append(m.overlays, layer)
	}
}

// WithMergeFunc overrides the default merge strategy.
func WithMergeFunc(fn MergeFunc) ManagerOption {
	return func(m *Manager) { m.mergeFn = fn }
}

// WithManagerLogger sets the logger for the manager.
func WithManagerLogger(l *slog.Logger) ManagerOption {
	return func(m *Manager) { m.logger = l }
}

// WithManagerClock sets the clock used for watch re-establishment backoff.
// Defaults to clock.System. Tests inject clocktest.FakeClock.
func WithManagerClock(c clock.Clock) ManagerOption {
	return func(m *Manager) {
		if c != nil {
			m.clk = c
		}
	}
}

// NewManager creates a Manager with the given base layer and options.
func NewManager(base Layer, opts ...ManagerOption) *Manager {
	m := &Manager{
		base:           base,
		mergeFn:        DefaultMerge,
		clk:            clock.System,
		configs:        make(map[string]*ports.BridgeConfig),
		watchErrs:      make(map[string]error),
		appliedVersion: -1,
		runningVersion: -1,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Load loads from the base, then each overlay in order, merging with
// MergeFunc. Only the final merged result is validated via
// config.Validate (individual layers are not validated independently
// since a layer may be intentionally incomplete).
func (m *Manager) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	baseCfg, err := m.base.Loader.Load(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.configs[m.base.Name] = baseCfg
	m.mu.Unlock()

	merged := baseCfg
	for _, ol := range m.overlays {
		olCfg, err := ol.Loader.Load(ctx)
		if err != nil {
			return nil, err
		}

		m.mu.Lock()
		m.configs[ol.Name] = olCfg
		m.mu.Unlock()

		merged, err = m.mergeFn(merged, olCfg)
		if err != nil {
			return nil, err
		}
	}

	if err := Validate(merged); err != nil {
		return nil, err
	}

	m.recordAppliedVersion(merged)
	return merged, nil
}

// Watch starts watching all layers that have a ports.Watcher. On any change
// it re-loads that layer, re-merges all layers, validates the merged
// result, and emits it on the returned channel. Invalid merged configs
// are logged and dropped (not emitted).
//
// Boot vs steady-state (Finding 1): every layer watcher is established
// SYNCHRONOUSLY here. A first-attempt establishment failure is FATAL and
// returned as *WatchStartError so the composition root exits non-zero instead
// of running blind. Once past boot, a watcher that dies is retried with
// backoff and surfaced via WatchErrors; the change channel is NEVER closed on
// a watch failure. It closes only when ctx is cancelled or Stop is called.
func (m *Manager) Watch(ctx context.Context) (<-chan *ports.BridgeConfig, error) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil, errAlreadyRunning
	}
	m.running = true
	m.stopping = false
	m.stopCh = make(chan struct{})
	m.doneCh = make(chan struct{})
	stopCh := m.stopCh
	doneCh := m.doneCh
	m.watchErrs = make(map[string]error)
	m.mu.Unlock()

	watchCtx, cancel := context.WithCancel(ctx)

	established, err := m.establishInitialWatchers(watchCtx)
	if err != nil {
		cancel()
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
		// doneCh was created but watchLoop will not run; close it so a
		// concurrent Stop waiting on it does not block forever.
		close(doneCh)
		return nil, err
	}

	out := make(chan *ports.BridgeConfig, 1)
	go m.watchLoop(watchCtx, cancel, out, stopCh, doneCh, established)
	return out, nil
}

// establishedWatch is a layer whose change channel has been opened once.
type establishedWatch struct {
	layer Layer
	ch    <-chan *ports.BridgeConfig
}

// establishInitialWatchers opens every watchable layer's change channel
// exactly once. A failure is a boot error (fatal): watching is a hard boot
// requirement, so the caller must fail loudly rather than run blind.
func (m *Manager) establishInitialWatchers(ctx context.Context) ([]establishedWatch, error) {
	layers := m.watchableLayers()
	established := make([]establishedWatch, 0, len(layers))
	for _, layer := range layers {
		ch, err := layer.Watcher.Watch(ctx)
		if err != nil {
			return nil, &WatchStartError{Layer: layer.Name, Err: err}
		}
		established = append(established, establishedWatch{layer: layer, ch: ch})
	}
	return established, nil
}

// watchableLayers returns the base plus every overlay that has a Watcher.
func (m *Manager) watchableLayers() []Layer {
	var out []Layer
	if m.base.Watcher != nil {
		out = append(out, m.base)
	}
	for _, ol := range m.overlays {
		if ol.Watcher != nil {
			out = append(out, ol)
		}
	}
	return out
}

// Stop stops all active watchers and waits for the watch loop to exit.
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	doneCh := m.doneCh
	if !m.stopping {
		m.stopping = true
		close(m.stopCh)
	}
	m.mu.Unlock()

	<-doneCh // both concurrent callers wait for watchLoop goroutine to exit

	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
}

// WatchDegraded reports whether any config layer's change watcher is
// currently failing to (re)establish. When true, live reconfiguration for at
// least one layer is unavailable and the manager is retrying with backoff;
// the last good config keeps serving. Safe for concurrent use.
func (m *Manager) WatchDegraded() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.watchErrs) > 0
}

// WatchErrors returns a snapshot of the current per-layer watch
// establishment errors (empty when healthy). Safe for concurrent use.
func (m *Manager) WatchErrors() map[string]error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.watchErrs)
}

func (m *Manager) setWatchError(layer string, err error) {
	m.mu.Lock()
	m.watchErrs[layer] = err
	m.mu.Unlock()
}

func (m *Manager) clearWatchError(layer string) {
	m.mu.Lock()
	delete(m.watchErrs, layer)
	m.mu.Unlock()
}

// AppliedVersion reports the DESIRED ports.BridgeConfig.Version this instance
// last merged, validated, and EMITTED downstream, and whether any config has
// been emitted yet. It is a per-instance convergence signal for detecting
// cross-instance config-version divergence.
//
// IMPORTANT: "emitted downstream" is NOT the same as "running". AppliedVersion
// advances the instant a merged config is validated and pushed to the applier,
// which is BEFORE the runtime swap that actually starts serving it. A swap can
// still fail (build/start/route validation) and the supervisor then recovers
// the PREVIOUS runtime. Use RunningVersion for the version the applier has
// CONFIRMED, and ReconfigurePending / LastApplyError to detect a desired config
// that has not (yet) taken effect. Reading AppliedVersion alone can report a
// live reload as active while traffic is still served by the old runtime
// (finding: desired-ahead-of-applied).
//
// ponytail: this is OBSERVATION ONLY. GoBridge deliberately does not coordinate
// config versions across the cluster (no version barrier, no cluster rollback) —
// a failed merge/validation keeps the prior config locally, so nodes can diverge
// indefinitely. Surfacing the applied version (and logging every change in
// recordAppliedVersion) lets operators alert on skew externally; building
// distributed consensus here is out of scope. Safe for concurrent use.
func (m *Manager) AppliedVersion() (version int, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appliedVersion, m.appliedVersion >= 0
}

// RunningVersion reports the ports.BridgeConfig.Version the downstream applier
// (supervisor) last CONFIRMED it applied via NotifyApplyResult, and whether any
// apply has been confirmed. Unlike AppliedVersion (the last EMITTED/desired
// version), this reflects the config actually serving traffic: after a failed
// runtime swap it stays at the previous, still-running version. Safe for
// concurrent use.
func (m *Manager) RunningVersion() (version int, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runningVersion, m.runningVersion >= 0
}

// ReconfigurePending reports whether the last DESIRED (emitted) config has not
// been confirmed applied by the runtime — i.e. the desired and running CONTENT
// fingerprints differ after at least one apply result has been reported for the
// current desired. It is the manager-side signal that a live reload is in-flight
// or, after a failed swap, stuck: the source/cache moved forward but the runtime
// did not.
//
// It is keyed on a content fingerprint, NOT on BridgeConfig.Version: a content
// change that reuses the operator version (e.g. an external file edit that does
// not bump version) still diverges here, where a Version comparison would falsely
// read as converged (finding: desired/running keyed on non-unique Version).
//
// It stays false until the first NotifyApplyResult so an unwired composition
// root (no acknowledgement path) is not reported as permanently pending. Safe
// for concurrent use.
func (m *Manager) ReconfigurePending() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.desiredHashErr != nil {
		// The desired config cannot be fingerprinted, so convergence can never be
		// PROVEN. Fail closed: report pending rather than let an unhashable desired
		// masquerade as converged (a zero fingerprint must never read as a valid,
		// matchable value — finding: marshal error collapses to a zero fingerprint).
		return true
	}
	return m.applyResultSeen && m.desiredFingerprint != m.runningFingerprint
}

// LastApplyError returns the error from the most recent FAILED apply result
// (nil once a later apply is confirmed, or before any result is reported). It is
// the diagnostic companion to ReconfigurePending: why the desired config is not
// running. Safe for concurrent use.
func (m *Manager) LastApplyError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastApplyErr
}

// NotifyApplyResult records the outcome of the downstream applier's attempt to
// make a previously-emitted (desired) config the RUNNING config. The applier
// passes back the EXACT config it attempted (the one delivered on the change
// channel) so the manager can correlate the result with the desired config by
// CONTENT — never by the operator-controlled, non-unique BridgeConfig.Version.
// It closes the desired-vs-applied gap that let the manager/source run ahead of
// the actual runtime after a failed swap:
//
//   - err == nil  → the runtime CONFIRMED it is serving cfg; RunningVersion
//     advances to cfg.Version, the running fingerprint becomes cfg's, and any
//     prior apply error is cleared.
//   - err != nil  → the swap DEFINITIVELY failed and the applier recovered the
//     previous runtime; the running fingerprint is left behind (still the old,
//     running config), lastApplyErr records why, and ReconfigurePending now
//     reports the divergence so operators/health see that the desired config is
//     NOT live.
//
// A result whose content does NOT match the current desired config is for a
// config the manager has already superseded — a late or out-of-order ack. It is
// IGNORED: acting on it would regress the running config to an old version or
// resurrect a stale error even though a newer config is already running
// (finding: stale acks regress running).
//
// Callers MUST only report definitive outcomes here. A committed-but-unconfirmed
// result (ports.ErrApplyInFlight — bridge paused/deferred, or a swap still
// in-flight past the apply deadline) is neither success nor failure: do NOT call
// NotifyApplyResult for it, so the pending/divergence state is preserved until a
// real result is known. A nil cfg is ignored. Safe for concurrent use.
func (m *Manager) NotifyApplyResult(cfg *ports.BridgeConfig, err error) {
	if cfg == nil {
		return
	}
	m.mu.Lock()
	if cfg != m.desiredConfig {
		// Correlate by the EXACT pointer the manager emitted (echoed back by the
		// applier via SwapEvent.NewConfig). A different pointer is a result for a
		// SUPERSEDED emit — a late/out-of-order ack (e.g. an A→B→A flap where a
		// delayed ack for the first A lands after the second A was emitted) or a
		// config that did not originate from this manager (an in-band admin swap).
		// Ignoring it stops a stale ack from regressing running to an old
		// generation whose CONTENT happens to match the current desired; content
		// hashing alone cannot make this distinction (identical content ⇒ identical
		// hash — finding: stale acks regress running).
		m.mu.Unlock()
		if m.logger != nil {
			m.logger.Debug("config manager: ignoring apply result for a superseded or foreign config "+
				"(not the current desired emit)", "acked_version", cfg.Version)
		}
		return
	}
	m.applyResultSeen = true
	desired := m.appliedVersion
	unverifiable := false
	switch {
	case err != nil:
		// Definitive failure: the applier recovered the previous runtime. Leave
		// runningFingerprint behind (still the old, running config) so
		// ReconfigurePending surfaces the divergence.
		m.lastApplyErr = err
	case m.desiredHashErr != nil:
		// Success reported for a desired we cannot fingerprint: we cannot PROVE
		// the runtime converged on it, so do NOT advance runningFingerprint.
		// desiredHashErr keeps ReconfigurePending true (fail closed).
		unverifiable = true
		m.lastApplyErr = fmt.Errorf("config manager: applied config cannot be fingerprinted, "+
			"convergence unverifiable: %w", m.desiredHashErr)
	default:
		// Success: the current desired is now the running config. cfg IS the
		// desired pointer, so its content is desiredFingerprint (computed once at
		// emit); copy it across rather than re-hash.
		m.runningVersion = cfg.Version
		m.runningFingerprint = m.desiredFingerprint
		m.lastApplyErr = nil
	}
	running := m.runningVersion
	m.mu.Unlock()

	if m.logger == nil {
		return
	}
	switch {
	case err != nil:
		m.logger.Error("config manager: runtime apply FAILED; desired config is NOT running "+
			"(desired != applied — traffic served by the previous runtime until re-driven or reverted)",
			"desired_version", desired, "running_version", running, "error", err)
	case unverifiable:
		m.logger.Warn("config manager: runtime reported the config applied, but it cannot be "+
			"fingerprinted so convergence cannot be verified (treated as still pending)",
			"running_version", cfg.Version)
	default:
		m.logger.Info("config manager: runtime confirmed config applied",
			"running_version", cfg.Version)
	}
}

// AdoptRunning reconciles the RUNNING state to a config the manager did NOT emit.
// A coordinated cluster rollout creates exactly this case: the barrier applies the
// applier's frozen candidate, or the durable committed artifact decoded at
// boot/reconcile — a DIFFERENT pointer than the desired emit, which
// NotifyApplyResult drops as foreign (it correlates by that exact pointer). Left
// unreconciled, runningFingerprint stays behind and ReconfigurePending — and thus
// deep-health Degraded — latches true forever though the member is converged.
//
// It advances runningVersion/runningFingerprint to cfg and clears any prior apply
// error WITHOUT the emit-pointer correlation. When cfg matches the current desired
// content this clears the pending signal (the ordinary rollout: the cohort
// committed the operator's change and this member now runs it). When it does not —
// a boot-time substitution to the committed config while the source still holds a
// rejected candidate — the divergence stays visible, which is correct: the
// operator must reconcile the source.
//
// A composition root calls this AFTER a confirmed barrier-driven swap, with the
// config the runtime is actually serving. A nil cfg is ignored; an unhashable cfg
// fails closed exactly like NotifyApplyResult (running is left behind and the
// unverifiable convergence is surfaced). Safe for concurrent use.
func (m *Manager) AdoptRunning(cfg *ports.BridgeConfig) {
	if cfg == nil {
		return
	}
	fp, hashErr := configFingerprint(cfg)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyResultSeen = true
	if hashErr != nil {
		// Cannot fingerprint the adopted config, so convergence cannot be PROVEN:
		// leave runningFingerprint behind and surface the error (fail closed).
		m.lastApplyErr = fmt.Errorf("config manager: barrier-applied config cannot be fingerprinted, "+
			"convergence unverifiable: %w", hashErr)
		return
	}
	m.runningVersion = cfg.Version
	m.runningFingerprint = fp
	m.lastApplyErr = nil
}

// configFingerprint returns a stable content fingerprint of the FULL logical
// config, used to detect whether the running config still matches the desired
// one WITHOUT relying on the operator-controlled, non-unique BridgeConfig.Version.
//
// It hashes two things into one SHA-256, over the REVEALED config (secrets
// included) so a secret-only edit is detected:
//
//  1. the top-level blueprint via encoding/json — structure plus every
//     non-plugin field (HTTP admin keys, cluster endpoints, routing, ...). This
//     step DROPS every typed PluginConfig, because the `Config` fields are tagged
//     json:"-" on the blueprint (ports.blueprint.go);
//  2. each decoded PluginConfig's options, in a fixed traversal order, so a
//     change confined to a plugin's options (or a plugin secret) at an unchanged
//     Version still changes the fingerprint (finding: plugin options omitted).
//
// encoding/json emits map keys in sorted order and struct fields in declaration
// order, so equal content always produces equal bytes. It returns an error when
// any part cannot be marshalled (e.g. a NaN/Inf reached via ConditionDef.Value
// any): callers MUST treat that as "cannot fingerprint" and fail closed — never
// substitute a zero fingerprint, which would falsely compare equal to another
// unhashable config and read as converged (finding: marshal error collapses to a
// zero fingerprint).
func configFingerprint(cfg *ports.BridgeConfig) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	if cfg == nil {
		return out, nil
	}
	h := sha256.New()
	enc := json.NewEncoder(h)
	if err := enc.Encode(shared.RevealSecrets(cfg)); err != nil {
		return out, fmt.Errorf("config manager: fingerprint bridge config: %w", err)
	}
	if err := forEachPluginConfig(cfg, func(pc ports.PluginConfig) error {
		// enc.Encode writes each value followed by a newline, so the ordered
		// stream of plugin payloads is self-delimiting. A nil PluginConfig encodes
		// as "null", preserving its structural position.
		if err := enc.Encode(shared.RevealSecrets(pc)); err != nil {
			return fmt.Errorf("config manager: fingerprint plugin config: %w", err)
		}
		return nil
	}); err != nil {
		return [sha256.Size]byte{}, err
	}
	h.Sum(out[:0])
	return out, nil
}

// forEachPluginConfig visits every decoded PluginConfig on the blueprint in a
// fixed, deterministic order (matching the parser's wire projection). The order
// is part of the fingerprint contract: two configs that differ only in the
// position of an otherwise-identical plugin option must fingerprint differently.
func forEachPluginConfig(cfg *ports.BridgeConfig, fn func(ports.PluginConfig) error) error {
	for _, sc := range []*ports.StoreConfig{cfg.Stores.Lease, cfg.Stores.Outbox, cfg.Stores.DLQ, cfg.Stores.ManagedSubscriptions} {
		if sc == nil {
			continue
		}
		if err := fn(sc.Config); err != nil {
			return err
		}
	}
	for i := range cfg.Sessions {
		if err := fn(cfg.Sessions[i].Config); err != nil {
			return err
		}
	}
	for i := range cfg.Receivers {
		if err := fn(cfg.Receivers[i].Config); err != nil {
			return err
		}
		for j := range cfg.Receivers[i].Topics {
			if err := fn(cfg.Receivers[i].Topics[j].Config); err != nil {
				return err
			}
		}
	}
	for i := range cfg.Senders {
		if err := fn(cfg.Senders[i].Config); err != nil {
			return err
		}
	}
	for i := range cfg.Bindings {
		if err := fn(cfg.Bindings[i].Config); err != nil {
			return err
		}
	}
	return nil
}

// recordAppliedVersion stamps the DESIRED config of a just-emitted merged config
// and logs any CONTENT change so cross-instance divergence is observable. cfg is
// the config the manager has committed to downstream (Load return / watch emit).
// This records what the manager ASKED to be applied, not what the runtime has
// confirmed running — see NotifyApplyResult / RunningVersion for the latter. The
// desired fingerprint is keyed on content, so a change that reuses the operator
// Version still supersedes the previous desired.
func (m *Manager) recordAppliedVersion(cfg *ports.BridgeConfig) {
	if cfg == nil {
		return
	}
	fp, hashErr := configFingerprint(cfg)
	m.mu.Lock()
	prevVersion := m.appliedVersion
	prevHashErr := m.desiredHashErr
	m.appliedVersion = cfg.Version
	// Track the EXACT emitted pointer so NotifyApplyResult can correlate its ack
	// by identity (distinguishes an A→B→A flap that content hashing cannot).
	m.desiredConfig = cfg
	m.desiredHashErr = hashErr
	var contentChanged bool
	switch {
	case hashErr != nil:
		// Cannot fingerprint this desired (e.g. a NaN/Inf in a ConditionDef.Value).
		// Treat it as a new, unconfirmable desired: ReconfigurePending short-circuits
		// on desiredHashErr, so leave the (now-stale) fingerprint untouched.
		contentChanged = true
	default:
		contentChanged = prevHashErr != nil || fp != m.desiredFingerprint
		m.desiredFingerprint = fp
	}
	if contentChanged {
		// A newly-emitted desired config supersedes the previous one, so an
		// error recorded for that prior desired no longer describes the CURRENT
		// desired's state (which has not been applied yet). Clear it so
		// LastApplyError only ever reflects the current desired.
		m.lastApplyErr = nil
	}
	m.mu.Unlock()
	if hashErr != nil {
		if m.logger != nil {
			m.logger.Error("config manager: desired config cannot be fingerprinted; "+
				"convergence is unverifiable (treated as permanently pending until replaced)",
				"desired_version", cfg.Version, "error", hashErr)
		}
		return
	}
	if m.logger == nil || !contentChanged {
		return
	}
	// Log on any CONTENT change, INCLUDING one that leaves BridgeConfig.Version
	// unchanged — exactly the case a Version-keyed check would miss.
	m.logger.Info("config manager: desired config changed (emitted downstream); "+
		"compare across instances to detect cluster divergence "+
		"(reconfiguration is per-instance, not cluster-coordinated)",
		"previous_version", prevVersion, "desired_version", cfg.Version,
		"version_unchanged", prevVersion == cfg.Version)
}

func (m *Manager) watchLoop(
	ctx context.Context,
	cancel context.CancelFunc,
	out chan *ports.BridgeConfig,
	stopCh <-chan struct{},
	doneCh chan struct{},
	initial []establishedWatch,
) {
	defer func() {
		cancel()
		close(out)
		close(doneCh)
	}()

	type layerEvent struct {
		name string
		cfg  *ports.BridgeConfig
	}

	fanIn := make(chan layerEvent, 4)
	var wg sync.WaitGroup

	// One supervisor goroutine per watchable layer. Each drains its change
	// channel and, when that channel closes WITHOUT ctx cancellation (the
	// underlying watcher died), re-establishes it with backoff. The
	// supervisors only exit on ctx cancellation, so fanIn (and therefore out)
	// is never closed because of a watch failure — that was the exit-0 bug.
	for _, ew := range initial {
		wg.Add(1)
		go func(ew establishedWatch) {
			defer wg.Done()
			name := ew.layer.Name
			m.superviseLayerWatch(ctx, ew.layer, ew.ch, func(cfg *ports.BridgeConfig) bool {
				select {
				case fanIn <- layerEvent{name: name, cfg: cfg}:
					return true
				case <-ctx.Done():
					return false
				}
			})
		}(ew)
	}

	go func() {
		wg.Wait()
		close(fanIn)
	}()

	for {
		select {
		case <-stopCh:
			cancel()
			return
		case <-ctx.Done():
			return
		case ev, ok := <-fanIn:
			if !ok {
				// Only reachable after ctx cancellation (all supervisors
				// exited). A watch failure never closes fanIn.
				return
			}
			// Validate the merged result BEFORE caching this layer's new
			// value. Caching first and validating after let a single poisoned
			// layer stick in the cache, so every later good update from another
			// layer re-merged against the poison and was dropped forever (one
			// bad layer blocked all future good updates). rebuildWith splices
			// ev.cfg in for the merge but does not commit it to the cache.
			merged, err := m.rebuildWith(ctx, ev.name, ev.cfg)
			if err != nil {
				// Keep the previous good value for ev.name so later good
				// updates from OTHER layers still merge cleanly. Surface the
				// rejected update as a degraded signal for operators.
				m.setWatchError(ev.name, fmt.Errorf("config manager: layer %q update rejected: %w", ev.name, err))
				if m.logger != nil {
					m.logger.Error("config manager: layer update rejected (merge/validate failed); "+
						"keeping last good config", "trigger", ev.name, "error", err)
				}
				continue
			}

			// Merge validated: now it is safe to commit the new layer value and
			// clear any prior rejection recorded for this layer.
			m.mu.Lock()
			m.configs[ev.name] = ev.cfg
			m.mu.Unlock()
			m.clearWatchError(ev.name)

			// Record the desired config BEFORE publishing it. The consumer
			// (applier/supervisor) may apply and acknowledge the moment it reads
			// from out; NotifyApplyResult correlates that ack against the desired
			// fingerprint, so the fingerprint MUST already be stamped or the ack
			// would be mistaken for a superseded config and dropped. Recording
			// first also means "desired" is set no later than the config becomes
			// observable downstream.
			m.recordAppliedVersion(merged)

			// Drain stale config so the consumer always gets the latest.
			select {
			case <-out:
			default:
			}
			out <- merged
		}
	}
}

// superviseLayerWatch drains a layer's change channel and re-establishes it
// with capped backoff whenever it closes before ctx is cancelled. forward
// delivers one config event and reports whether the loop should continue.
func (m *Manager) superviseLayerWatch(
	ctx context.Context,
	layer Layer,
	ch <-chan *ports.BridgeConfig,
	forward func(*ports.BridgeConfig) bool,
) {
	backoff := watchRetryInitial
	established := true
	for {
		// Drain the current channel until it closes or ctx is cancelled.
		drained := false
		for !drained {
			select {
			case <-ctx.Done():
				return
			case cfg, ok := <-ch:
				if !ok {
					drained = true
					break
				}
				if !forward(cfg) {
					return
				}
				// Healthy activity: reset backoff.
				backoff = watchRetryInitial
			}
		}

		// Steady-state watch death (NOT a shutdown): record degraded state
		// and re-establish with backoff. The runtime keeps serving throughout.
		// Only record the generic "channel closed" error when a LIVE watcher
		// just died: on the retry path ch is still the stale closed channel,
		// and overwriting here would clobber the more specific establishment
		// error recorded below — the one operators need to diagnose the outage.
		if layer.Watcher == nil {
			return
		}
		if established {
			m.setWatchError(layer.Name, errWatchEnded)
			if m.logger != nil {
				m.logger.Error("config manager: layer watcher ended; re-establishing with backoff "+
					"(runtime keeps serving last good config)", "layer", layer.Name, "retry_in", backoff)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-m.clk.After(backoff):
		}

		newCh, err := layer.Watcher.Watch(ctx)
		if err != nil {
			m.setWatchError(layer.Name, err)
			if m.logger != nil {
				m.logger.Error("config manager: re-establishing layer watcher failed; will retry",
					"layer", layer.Name, "error", err, "retry_in", backoff)
			}
			backoff = nextBackoff(backoff)
			// ch is still the (closed) old channel; the drain loop above will
			// return immediately and we retry after the next backoff.
			established = false
			continue
		}
		m.clearWatchError(layer.Name)
		ch = newCh
		backoff = watchRetryInitial
		established = true
	}
}

func nextBackoff(d time.Duration) time.Duration {
	next := d * 2
	if next > watchRetryMax {
		return watchRetryMax
	}
	return next
}

// rebuildWith re-merges all layers from the cached per-layer configs, splicing
// overrideCfg in for overrideName WITHOUT committing it to the cache. A layer
// whose cached config is nil is loaded and cached (a genuine load, distinct
// from the candidate value under test). The merged result is validated; on any
// merge or validation error the candidate is left uncommitted so a single bad
// layer update cannot poison the cache and block later good updates from other
// layers.
func (m *Manager) rebuildWith(ctx context.Context, overrideName string, overrideCfg *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	// cfgFor returns the candidate value for the triggering layer and the
	// cached value for every other layer.
	cfgFor := func(name string) *ports.BridgeConfig {
		if name == overrideName {
			return overrideCfg
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.configs[name]
	}

	baseCfg := cfgFor(m.base.Name)
	if baseCfg == nil {
		var err error
		baseCfg, err = m.base.Loader.Load(ctx)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.configs[m.base.Name] = baseCfg
		m.mu.Unlock()
	}

	merged := baseCfg
	for _, ol := range m.overlays {
		olCfg := cfgFor(ol.Name)
		if olCfg == nil {
			var err error
			olCfg, err = ol.Loader.Load(ctx)
			if err != nil {
				return nil, err
			}
			m.mu.Lock()
			m.configs[ol.Name] = olCfg
			m.mu.Unlock()
		}

		var err error
		merged, err = m.mergeFn(merged, olCfg)
		if err != nil {
			return nil, err
		}
	}

	if err := Validate(merged); err != nil {
		return nil, err
	}

	return merged, nil
}

var (
	errAlreadyRunning = errorf("config manager: already running")
	errWatchEnded     = errorf("config manager: layer watcher change channel closed")
)

type configError string

func errorf(msg string) configError { return configError(msg) }

func (e configError) Error() string { return string(e) }
