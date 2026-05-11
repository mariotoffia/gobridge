package servicebus

import (
	"errors"

	"github.com/mariotoffia/gobridge/ports"
)

// Register installs this adapter's PluginConfig decoder under the
// short and fully-qualified discriminators on the supplied registry.
func Register(reg *ports.Registry) error {
	dec := func(raw ports.RawConfig) (ports.PluginConfig, error) {
		var c Config
		if raw != nil {
			if err := raw.Decode(&c); err != nil {
				return nil, err
			}
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		return &c, nil
	}
	return errors.Join(
		reg.Register("servicebus", dec),
		reg.Register("azure.servicebus", dec),
	)
}
