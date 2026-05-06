package servicebus

import "github.com/mariotoffia/gobridge/ports"

func init() {
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
	ports.DefaultRegistry.Register("servicebus", dec)
	ports.DefaultRegistry.Register("azure.servicebus", dec)
}
