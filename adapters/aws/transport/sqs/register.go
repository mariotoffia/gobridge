package sqs

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
	// Register both the short discriminator (matches the supervisor's
	// transport map and the conventional YAML form `transport: sqs`)
	// and the long form (`aws.sqs`) used by Kind() for documentation
	// and any consumer that prefers a fully-qualified name.
	ports.DefaultRegistry.Register("sqs", dec)
	ports.DefaultRegistry.Register("aws.sqs", dec)
}
