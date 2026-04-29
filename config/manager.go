package config

import (
	"context"
	"log/slog"
	"sync"

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

	mu      sync.Mutex
	configs map[string]*ports.BridgeConfig // cached per-layer configs
	stopCh  chan struct{}
	doneCh  chan struct{} // closed when watchLoop exits
	running bool
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

// NewManager creates a Manager with the given base layer and options.
func NewManager(base Layer, opts ...ManagerOption) *Manager {
	m := &Manager{
		base:    base,
		mergeFn: DefaultMerge,
		configs: make(map[string]*ports.BridgeConfig),
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

	return merged, nil
}

// Watch starts watching all layers that have a ports.Watcher. On any change
// it re-loads that layer, re-merges all layers, validates the merged
// result, and emits it on the returned channel. Invalid merged configs
// are logged and dropped (not emitted). The channel is closed when ctx
// is cancelled or Stop is called.
func (m *Manager) Watch(ctx context.Context) (<-chan *ports.BridgeConfig, error) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil, errAlreadyRunning
	}
	m.running = true
	m.stopCh = make(chan struct{})
	m.doneCh = make(chan struct{})
	stopCh := m.stopCh
	doneCh := m.doneCh
	m.mu.Unlock()

	out := make(chan *ports.BridgeConfig, 1)
	ctx, cancel := context.WithCancel(ctx)

	go m.watchLoop(ctx, cancel, out, stopCh, doneCh)
	return out, nil
}

// Stop stops all active watchers and waits for the watch loop to exit.
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	close(m.stopCh)
	doneCh := m.doneCh
	m.mu.Unlock()

	<-doneCh // wait for watchLoop goroutine to exit

	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
}

func (m *Manager) watchLoop(ctx context.Context, cancel context.CancelFunc, out chan *ports.BridgeConfig, stopCh <-chan struct{}, doneCh chan struct{}) {
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

	startLayerWatch := func(layer Layer) {
		if layer.Watcher == nil {
			return
		}
		ch, err := layer.Watcher.Watch(ctx)
		if err != nil {
			if m.logger != nil {
				m.logger.Warn("config manager: failed to start watcher",
					"layer", layer.Name, "error", err)
			}
			return
		}
		wg.Add(1)
		go func(name string, ch <-chan *ports.BridgeConfig) {
			defer wg.Done()
			for cfg := range ch {
				select {
				case fanIn <- layerEvent{name: name, cfg: cfg}:
				case <-ctx.Done():
					return
				}
			}
		}(layer.Name, ch)
	}

	startLayerWatch(m.base)
	for _, ol := range m.overlays {
		startLayerWatch(ol)
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
				return
			}
			m.mu.Lock()
			m.configs[ev.name] = ev.cfg
			m.mu.Unlock()

			merged, err := m.rebuild(ctx)
			if err != nil {
				if m.logger != nil {
					m.logger.Warn("config manager: rebuild failed", "trigger", ev.name, "error", err)
				}
				continue
			}

			// Drain stale config so the consumer always gets the latest.
			select {
			case <-out:
			default:
			}
			out <- merged
		}
	}
}

// rebuild re-merges all layers from cached configs, re-loading any
// layer whose cached config is nil.
func (m *Manager) rebuild(ctx context.Context) (*ports.BridgeConfig, error) {
	m.mu.Lock()
	baseCfg := m.configs[m.base.Name]
	m.mu.Unlock()

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
		m.mu.Lock()
		olCfg := m.configs[ol.Name]
		m.mu.Unlock()

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

var errAlreadyRunning = errorf("config manager: already running")

type configError string

func errorf(msg string) configError { return configError(msg) }

func (e configError) Error() string { return string(e) }
