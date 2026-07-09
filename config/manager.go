package config

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
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
	appliedVersion int
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

// AppliedVersion reports the ports.BridgeConfig.Version this instance has last
// successfully merged, validated, and emitted downstream, and whether any config
// has been applied yet. It is a per-instance convergence signal for detecting
// cross-instance config-version divergence.
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

// recordAppliedVersion stamps the version of a just-emitted merged config and
// logs any change so cross-instance divergence is observable. cfg is the config
// the manager has committed to downstream (Load return / watch emit).
func (m *Manager) recordAppliedVersion(cfg *ports.BridgeConfig) {
	if cfg == nil {
		return
	}
	m.mu.Lock()
	prev := m.appliedVersion
	m.appliedVersion = cfg.Version
	m.mu.Unlock()
	if m.logger != nil && prev != cfg.Version {
		m.logger.Info("config manager: applied config version changed; "+
			"compare across instances to detect cluster divergence "+
			"(reconfiguration is per-instance, not cluster-coordinated)",
			"previous_version", prev, "applied_version", cfg.Version)
	}
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

			// Drain stale config so the consumer always gets the latest.
			select {
			case <-out:
			default:
			}
			out <- merged
			m.recordAppliedVersion(merged)
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
