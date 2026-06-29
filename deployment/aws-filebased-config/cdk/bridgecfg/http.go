package bridgecfg

import (
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// WithHTTPAdminAPI populates BridgeConfig.HTTP from the supplied
// HTTPAdminAPIOptions. Calling WithHTTPAdminAPI multiple times
// replaces any previously installed HTTP block — the bridge runtime
// supports a single admin/monitor pair so a "merge" semantics here
// would mask operator mistakes.
//
// AdminAPIKey and MonitorAPIKey are written verbatim. The plaintext
// scanner run from Build verifies the values are credential URIs
// rather than literals, so callers cannot accidentally bake an
// inline secret into the synthesized bridge.yaml.
func (b *Builder) WithHTTPAdminAPI(opts HTTPAdminAPIOptions) *Builder {
	b.cfg.HTTP = &ports.HTTPConfig{
		AdminAddr:     opts.AdminAddr,
		MonitorAddr:   opts.MonitorAddr,
		AdminAPIKey:   shared.NewSecret(opts.AdminAPIKey),
		MonitorAPIKey: shared.NewSecret(opts.MonitorAPIKey),
		CORSOrigins:   opts.CORSOrigins,
	}
	return b
}
