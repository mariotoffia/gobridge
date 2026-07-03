package paho

import (
	"errors"

	"github.com/mariotoffia/gobridge/ports"
)

// Register installs this adapter's PluginConfig decoder under the
// short ("mqtt") and fully-qualified ("mqtt.paho") discriminators.
//
// The decoder decodes into a DefaultConfig()-pre-filled value so every
// documented default (keep_alive 30, connect_timeout 30s,
// reconnect_timeout 30s, sender qos 1, sender timeout 30s, …) applies
// on the typed YAML path, while explicit values — including explicit
// zeros such as `qos: 0` and `keep_alive: 0` — override them
// (mapstructure only assigns keys present in the input).
func Register(reg *ports.Registry) error {
	dec := func(raw ports.RawConfig) (ports.PluginConfig, error) {
		c := DefaultConfig()
		if raw != nil {
			if err := raw.Decode(&c); err != nil {
				return nil, err
			}
		}
		c.Session.normalizeBrokerURLs()
		if err := c.Validate(); err != nil {
			return nil, err
		}
		return &c, nil
	}
	return errors.Join(
		reg.Register("mqtt", dec),
		reg.Register("mqtt.paho", dec),
	)
}
