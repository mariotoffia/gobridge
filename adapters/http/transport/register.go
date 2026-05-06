package transport

import (
	"fmt"

	"github.com/mariotoffia/gobridge/ports"
)

func init() {
	ports.DefaultRegistry.Register(Kind, decode)
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
