package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// watchErrorReason renders the config manager's per-layer watch failures as one
// operator-readable reason, or "" when every watcher is healthy. Layers are
// sorted so the same set of failures always produces the same string — map
// iteration order would otherwise make the /deephealth payload churn between
// probes and defeat any alert that matches on it.
func watchErrorReason(errs map[string]error) string {
	if len(errs) == 0 {
		return ""
	}
	layers := make([]string, 0, len(errs))
	for layer := range errs {
		layers = append(layers, layer)
	}
	slices.Sort(layers)
	parts := make([]string, len(layers))
	for i, layer := range layers {
		parts[i] = fmt.Sprintf("%s: %v", layer, errs[layer])
	}
	return "config watch degraded: " + strings.Join(parts, "; ")
}

// currentShutdownTimeout resolves the shutdown budget the process should honour
// RIGHT NOW: the running configuration's bridge.shutdown_timeout, falling back
// to the boot configuration when the supervisor has nothing active to report
// (it never built a runtime, or it is wedged).
//
// Reading it fresh matters because the budget is consumed at shutdown, long
// after boot: an operator who raises bridge.shutdown_timeout through a reload
// expects the HTTP drain and the final supervisor wait to use the new value.
// Reading the boot config instead would cut a deliberately lengthened drain
// short with a number the process no longer runs.
func currentShutdownTimeout(current func() *ports.BridgeConfig, boot *ports.BridgeConfig) time.Duration {
	if cfg := current(); cfg != nil {
		return cfg.Bridge.ShutdownTimeoutDuration()
	}
	return boot.Bridge.ShutdownTimeoutDuration()
}

// The listen addresses this root binds when the configuration omits them.
// httpTopologyRestartReason normalises both sides with them, so writing a
// default out explicitly is not reported as a change to a listener already
// bound there.
const (
	defaultAdminAddr   = ":8080"
	defaultMonitorAddr = ":8081"
)

// httpTopologyRestartReason reports why the running configuration's `http`
// block cannot take effect without a process restart, or "" when it matches the
// block the listeners were bound from.
//
// This composition root creates its admin and monitor servers ONCE, from the
// boot configuration. A reload — through the file watcher or the admin config
// API — validates and durably stores a changed `http` block, and then nothing
// happens: the listeners keep their original addresses, TLS material and CORS
// policy. Surfacing the divergence through deep health is what turns that
// silence into something an operator can see and act on.
//
// Only the fields BOUND INTO the listeners at startup count as topology. The
// API keys are deliberately excluded: they are read per request, so rotating
// one applies immediately and must not raise a restart signal.
func httpTopologyRestartReason(boot, running *ports.HTTPConfig) string {
	if running == nil || boot == nil {
		// Nothing configured at boot means no listeners exist to diverge from,
		// and a running config without an `http` block asks for nothing. Neither
		// state is actionable through a restart signal; the start-empty warning
		// already tells an operator that no HTTP server was created.
		return ""
	}
	changed := make([]string, 0, 5)
	for _, field := range []struct {
		name       string
		boot, live string
	}{
		{"admin_addr", orDefault(boot.AdminAddr, defaultAdminAddr), orDefault(running.AdminAddr, defaultAdminAddr)},
		{"monitor_addr", orDefault(boot.MonitorAddr, defaultMonitorAddr), orDefault(running.MonitorAddr, defaultMonitorAddr)},
		{"cors_origins", boot.CORSOrigins, running.CORSOrigins},
		{"tls_cert_file", boot.TLSCertFile, running.TLSCertFile},
		{"tls_key_file", boot.TLSKeyFile, running.TLSKeyFile},
	} {
		if field.boot != field.live {
			changed = append(changed, field.name)
		}
	}
	if len(changed) == 0 {
		return ""
	}
	return "http " + strings.Join(changed, ", ") +
		" changed since startup; HTTP listeners are bound once at startup, so restart the process to apply"
}

// orDefault returns value, or fallback when value is empty.
func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
