package bootstrap

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/config"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

type bridgeConfigRef struct {
	mu  sync.RWMutex
	cfg *config.BridgeConfig
}

func (r *bridgeConfigRef) Get() *config.BridgeConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

func (r *bridgeConfigRef) Set(cfg *config.BridgeConfig) {
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

func (r *apiKeysRef) MonitorKey() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.monitorKey
}

func stopRuntime(rt *goruntime.Runtime, cfg *config.BridgeConfig) error {
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
