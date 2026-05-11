package transport

import (
	"fmt"

	"github.com/mariotoffia/gobridge/ports"
)

// Register installs this adapter's PluginConfig decoder under the
// canonical "http" discriminator on the supplied registry.
func Register(reg *ports.Registry) error {
	return reg.Register(Kind, decode)
}

func decode(raw ports.RawConfig) (ports.PluginConfig, error) {
	var c Config
	if raw != nil {
		if err := raw.Decode(&c); err != nil {
			return nil, fmt.Errorf("http: decode config: %w", err)
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
