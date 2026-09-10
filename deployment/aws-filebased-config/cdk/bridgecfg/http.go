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
// AdminAPIKey and MonitorAPIKey may be literals or credential references.
// Their values are preserved in the config's redacting shared.Secret fields.
// Existing runtime key-presence and key-strength validation still applies.
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
