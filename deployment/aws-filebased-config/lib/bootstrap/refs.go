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

func stopRuntime(rt *goruntime.Runtime, cfg *ports.BridgeConfig) error {
	if rt == nil {
		return nil
	}

	drainTimeout := 30 * time.Second
	if cfg != nil {
		drainTimeout = cfg.Bridge.DrainTimeoutDuration()
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	return rt.Stop(stopCtx)
}
