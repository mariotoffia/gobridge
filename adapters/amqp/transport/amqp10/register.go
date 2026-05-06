package amqp10

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
	ports.DefaultRegistry.Register("amqp10", dec)
	ports.DefaultRegistry.Register("amqp.amqp10", dec)
}
