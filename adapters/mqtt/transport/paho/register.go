package paho

import (
	"errors"

	"github.com/mariotoffia/gobridge/ports"
)

// Register installs this adapter's PluginConfig decoder under the
// short ("mqtt") and fully-qualified ("mqtt.paho") discriminators.
func Register(reg *ports.Registry) error {
	dec := func(raw ports.RawConfig) (ports.PluginConfig, error) {
		var c Config
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
