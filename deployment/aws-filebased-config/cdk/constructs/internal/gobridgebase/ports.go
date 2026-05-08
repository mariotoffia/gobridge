package gobridgebase

import (
	"sort"
	"strconv"
	"strings"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// PortKind identifies what a derived port mapping serves.
type PortKind string

const (
	PortKindAdmin     PortKind = "admin"
	PortKindMonitor   PortKind = "monitor"
	PortKindTransport PortKind = "transport-http"
)

// PortMapping is the construct-agnostic projection of a derived
// container port. Order is deterministic across calls — mappings are
// emitted in (kind, port) order.
type PortMapping struct {
	Kind PortKind
	Port float64
}

// DerivePortMappings extracts the container ports the bridge runtime
// will listen on, derived solely from the parsed config and bootstrap
// (no hard-coded defaults beyond the package-level Default* constants
// from the infra module, which are themselves the runtime defaults).
//
// Rules:
//   - Admin port is always emitted (bootstrap.AdminAddr; HTTPConfig
//     overrides when set).
//   - Monitor port is emitted when bootstrap.MonitorAddr or
//     HTTPConfig.MonitorAddr is non-empty.
//   - Transport HTTP port is emitted iff at least one ReceiverDef in
//     cfg has Transport == "http"; the port itself comes from
//     bootstrap.TransportHTTPAddr (single shared listener — receivers
//     register routes against it).
func DerivePortMappings(cfg *ports.BridgeConfig, boot infra.BootstrapConfig) []PortMapping {
	boot = boot.Normalized()

	adminAddr := boot.AdminAddr
	monitorAddr := boot.MonitorAddr
	if cfg != nil && cfg.HTTP != nil {
		if cfg.HTTP.AdminAddr != "" {
			adminAddr = cfg.HTTP.AdminAddr
		}
		if cfg.HTTP.MonitorAddr != "" {
			monitorAddr = cfg.HTTP.MonitorAddr
		}
	}

	out := []PortMapping{}
	if p, ok := portFromAddr(adminAddr); ok {
		out = append(out, PortMapping{Kind: PortKindAdmin, Port: p})
	}
	if monitorAddr != "" {
		if p, ok := portFromAddr(monitorAddr); ok {
			out = append(out, PortMapping{Kind: PortKindMonitor, Port: p})
		}
	}
	if hasHTTPReceiver(cfg) {
		if p, ok := portFromAddr(boot.TransportHTTPAddr); ok {
			out = append(out, PortMapping{Kind: PortKindTransport, Port: p})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Port < out[j].Port
	})
	return out
}

func hasHTTPReceiver(cfg *ports.BridgeConfig) bool {
	if cfg == nil {
		return false
	}
	for _, r := range cfg.Receivers {
		if strings.EqualFold(r.Transport, "http") {
			return true
		}
	}
	return false
}

// portFromAddr parses ":8080" or "0.0.0.0:8080" into 8080. Returns
// false if the addr is empty, malformed, or specifies port 0 (which
// the runtime resolves dynamically and which is meaningless for an
// ECS port mapping).
func portFromAddr(addr string) (float64, bool) {
	if addr == "" {
		return 0, false
	}
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return 0, false
	}
	port, err := strconv.Atoi(addr[idx+1:])
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return float64(port), true
}
