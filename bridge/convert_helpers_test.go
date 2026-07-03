package bridge

import (
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// The error-swallowing convenience wrappers toRoutePolicy, toSessionConfig,
// and toDrainStrategy were removed from production convert.go (Finding 13):
// they had no non-test callers and hid conversion errors. They live here as
// test-only helpers so existing table-driven tests that feed known-good inputs
// stay concise; tests exercising the error paths call the *E variants directly.

func toRoutePolicy(r ports.RouteDef) routing.RoutePolicy {
	p, _ := toRoutePolicyE(r)
	return p
}

func toSessionConfig(rs *ports.RouteSessionDef) *session.Config {
	sc, _ := toSessionConfigE(rs)
	return sc
}

func toDrainStrategy(rs *ports.RouteSessionDef) persistence.DrainStrategy {
	ds, _ := toDrainStrategyE(rs)
	return ds
}
