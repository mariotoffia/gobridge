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
// will listen on, derived solely from the BootstrapConfig listen
// addresses (no hard-coded defaults beyond the package-level Default*
// constants from the infra module, which are themselves the runtime
// defaults).
//
// The BootstrapConfig is the SINGLE authoritative port source: the
// file-based runtime binds admin, monitor, and transport exclusively
// to boot.AdminAddr / boot.MonitorAddr / boot.TransportHTTPAddr (see
// lib/bootstrap App.startLocked → apiCfg.{Admin,Monitor}Addr and
// transportServer.Start(cfg.TransportHTTPAddr)). The bridge yaml
// `http:` block is DELIBERATELY NOT consulted here: this profile
// ignores it at runtime (lib/bootstrap.checkIgnoredHTTPBlock
// warns-and-ignores a bare `http:` block and fails closed on a TLS
// pair). Deriving ALB / target-group / health-check ports from the
// `http:` block would aim them at ports NOTHING listens on the moment
// an operator sets e.g. http.admin_addr — breaking health checks and
// failing deploys (c15-cdk-ports). Read the port only from bootstrap
// so the CDK and the runtime agree by construction.
//
// Rules:
//   - Admin port is always emitted from bootstrap.AdminAddr.
//   - Monitor port is emitted when bootstrap.MonitorAddr is non-empty.
//   - Transport HTTP port is emitted iff at least one ReceiverDef in
//     cfg has Transport == "http"; the port itself comes from
//     bootstrap.TransportHTTPAddr (single shared listener — receivers
//     register routes against it). cfg is consulted ONLY to detect
//     that at least one HTTP receiver exists, never for the port.
func DerivePortMappings(cfg *ports.BridgeConfig, boot infra.BootstrapConfig) []PortMapping {
	boot = boot.Normalized()

	adminAddr := boot.AdminAddr
	monitorAddr := boot.MonitorAddr

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
