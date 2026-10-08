package paho

import (
	"errors"

	"github.com/mariotoffia/gobridge/ports"
)

const (
	// ShortKind is the conventional YAML transport discriminator.
	ShortKind = "mqtt"
	// QualifiedKind is the fully-qualified Paho transport discriminator.
	QualifiedKind = "mqtt.paho"
)

// IsKind reports whether kind is one of the decoder/factory aliases owned by
// this adapter.
func IsKind(kind string) bool { return kind == ShortKind || kind == QualifiedKind }

// Register installs this adapter's PluginConfig decoder under the
// short ("mqtt") and fully-qualified ("mqtt.paho") discriminators.
//
// The decoder decodes into a DefaultConfig()-pre-filled value. Ordinary
// defaults (keep_alive, timeouts, sender QoS, …) materialize immediately, while
// receive_maximum and max_payload_bytes stay zero until NewSession applies
// their defaults.
func Register(reg *ports.Registry) error {
	dec := func(raw ports.RawConfig) (ports.PluginConfig, error) {
		c := DefaultConfig()
		if raw != nil {
			if err := raw.Decode(&c); err != nil {
				return nil, err
			}
		}
		c.Session.normalizeBrokerURLs()
		c.Session.normalizeDefaults()
		if err := c.Validate(); err != nil {
			return nil, err
		}
		return &c, nil
	}
	return errors.Join(
		reg.Register(ShortKind, dec),
		reg.Register(QualifiedKind, dec),
	)
}
