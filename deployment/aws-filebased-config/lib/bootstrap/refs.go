package bootstrap

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

type bridgeConfigRef struct {
	mu  sync.RWMutex
	cfg *ports.BridgeConfig
}

func (r *bridgeConfigRef) Get() *ports.BridgeConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

func (r *bridgeConfigRef) Set(cfg *ports.BridgeConfig) {
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
}

type runtimeRef struct {
	mu sync.RWMutex
	rt *goruntime.Runtime
}

func (r *runtimeRef) Get() *goruntime.Runtime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rt
}

func (r *runtimeRef) Set(rt *goruntime.Runtime) {
	r.mu.Lock()
	r.rt = rt
	r.mu.Unlock()
}

type apiKeysRef struct {
	mu         sync.RWMutex
	adminKey   string
	monitorKey string
}

func (r *apiKeysRef) Set(adminKey, monitorKey string) {
	r.mu.Lock()
	r.adminKey = adminKey
	r.monitorKey = monitorKey
	r.mu.Unlock()
}

func (r *apiKeysRef) AdminKey() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.adminKey
}

// AdminKeys returns the current admin API keys as name->key. It applies
// JSON-in-SSM detection to the stored raw value: a JSON object is parsed into
// named keys; any other value is the legacy single key under the name "admin".
// The stored raw value has ALWAYS passed resolveInputs (parseAdminKeys shape +
// httpapi.ValidateAdminKeys name/length rules) before installPlan stores it —
// at startup and on every reload alike — so this method only ever re-parses
// already-validated input. The dropped parseAdminKeys error is therefore a
// purely DEFENSIVE backstop, not a live path: it yields a nil map, which is
// safe to range (the httpapi server then matches no key and fails closed) and
// never panics.
func (r *apiKeysRef) AdminKeys() map[string]string {
	keys, _ := parseAdminKeys(r.AdminKey())
	return keys
}

func (r *apiKeysRef) MonitorKey() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.monitorKey
}

// stopRuntime stops rt with the configured drain timeout, derived from ctx
// so a caller-imposed deadline also bounds the drain. The process-shutdown
// path passes the app's shutdown context: previously the drain ran on a
// FRESH background budget stacked AFTER the app's own shutdown budget, so a
// slow teardown could take shutdown_timeout + drain_timeout (> 60s worst
// case) and blow through the Kubernetes termination grace period into a
// SIGKILL mid-drain. Reload paths pass context.Background(): a
// hot-reload drain is not bounded by process shutdown. Delivery guarantees
// survive either way (unsettled deliveries fall back to broker redelivery);
// the single budget is what keeps the drained-shutdown intent honest.
func stopRuntime(ctx context.Context, rt *goruntime.Runtime, cfg *ports.BridgeConfig) error {
	if rt == nil {
		return nil
	}

	drainTimeout := 30 * time.Second
	if cfg != nil {
		drainTimeout = cfg.Bridge.DrainTimeoutDuration()
	}

	stopCtx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()
	return rt.Stop(stopCtx)
}

// waitCtx runs wait on its own goroutine and reports whether it finished before
// ctx ended. The abandoned goroutine is harmless: every caller has already
// cancelled the work the wait is for, so it unwinds on its own — the point is
// that a stuck unwind must not consume the process shutdown budget.
func waitCtx(ctx context.Context, wait func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait()
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
