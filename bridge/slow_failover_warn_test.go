package bridge

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

const failoverWarnSubstr = "failover band"

func clusteredCfg() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			Cluster: &ports.ClusterConfig{Endpoints: map[string]string{"http": "10.0.0.1:8080"}},
		},
	}
}

// TestWarnSlowClusterFailover_FiresOnPinnedLooseTTL proves the F-1 advisory
// fires exactly once for a clustered exclusive session whose lease TTL exceeds
// the documented 30-60s failover band.
func TestWarnSlowClusterFailover_FiresOnPinnedLooseTTL(t *testing.T) {
	buf := &bytes.Buffer{}
	b := &Builder{
		cfg:    clusteredCfg(),
		logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	routeDef := ports.RouteDef{ID: "r1", Session: &ports.RouteSessionDef{SessionID: "s1"}}
	sc := &session.Config{LeaseTTL: 300 * time.Second}

	warned := map[string]bool{}
	b.warnSlowClusterFailover(routeDef, sc, warned)
	if got := strings.Count(buf.String(), failoverWarnSubstr); got != 1 {
		t.Fatalf("expected exactly one F-1 warning, got %d\nlog: %s", got, buf.String())
	}

	// A second route on the same session must be deduped.
	b.warnSlowClusterFailover(ports.RouteDef{ID: "r2", Session: &ports.RouteSessionDef{SessionID: "s1"}}, sc, warned)
	if got := strings.Count(buf.String(), failoverWarnSubstr); got != 1 {
		t.Fatalf("advisory must dedupe per session, got %d warnings\nlog: %s", got, buf.String())
	}
}

// TestWarnSlowClusterFailover_SilentInBand proves the advisory does NOT fire
// when the clustered session's lease TTL is within the 30-60s band — the case
// produced by the automatic 45s HA default for unpinned clustered sessions.
func TestWarnSlowClusterFailover_SilentInBand(t *testing.T) {
	buf := &bytes.Buffer{}
	b := &Builder{
		cfg:    clusteredCfg(),
		logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	routeDef := ports.RouteDef{ID: "r1", Session: &ports.RouteSessionDef{SessionID: "s1"}}
	sc := &session.Config{LeaseTTL: 45 * time.Second}

	b.warnSlowClusterFailover(routeDef, sc, map[string]bool{})
	if strings.Contains(buf.String(), failoverWarnSubstr) {
		t.Fatalf("in-band lease TTL must not warn\nlog: %s", buf.String())
	}
}

// TestWarnSlowClusterFailover_SilentSingleNode proves that a loose lease TTL on
// a NON-clustered (single-node) deployment is not warned: with no peer there is
// nothing to fail over to, so the failover band does not apply.
func TestWarnSlowClusterFailover_SilentSingleNode(t *testing.T) {
	buf := &bytes.Buffer{}
	b := &Builder{
		cfg:    &ports.BridgeConfig{}, // no cluster
		logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	routeDef := ports.RouteDef{ID: "r1", Session: &ports.RouteSessionDef{SessionID: "s1"}}
	sc := &session.Config{LeaseTTL: 300 * time.Second}

	b.warnSlowClusterFailover(routeDef, sc, map[string]bool{})
	if strings.Contains(buf.String(), failoverWarnSubstr) {
		t.Fatalf("single-node deployment must not warn on a loose lease TTL\nlog: %s", buf.String())
	}
}
