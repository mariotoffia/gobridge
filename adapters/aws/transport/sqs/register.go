package sqs

import (
	"errors"

	"github.com/mariotoffia/gobridge/ports"
)

// Register installs this adapter's PluginConfig decoder under the
// short ("sqs") and fully-qualified ("aws.sqs") discriminators.
// Composition roots call Register exactly once per registry.
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
	// Register both the short discriminator (matches the supervisor's
	// transport map and the conventional YAML form `transport: sqs`)
	// and the long form (`aws.sqs`) used by Kind() for documentation
	// and any consumer that prefers a fully-qualified name.
	return errors.Join(
		reg.Register("sqs", dec),
		reg.Register("aws.sqs", dec),
	)
}
