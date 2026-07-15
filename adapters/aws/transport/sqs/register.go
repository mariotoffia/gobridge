package sqs

import (
	"errors"

	"github.com/mariotoffia/gobridge/ports"
)

const (
	// ShortKind is the conventional YAML transport discriminator.
	ShortKind = "sqs"
	// QualifiedKind is the fully-qualified transport discriminator and Config.Kind value.
	QualifiedKind = "aws.sqs"
)

// IsKind reports whether kind is one of the decoder/factory aliases owned by
// this adapter.
func IsKind(kind string) bool { return kind == ShortKind || kind == QualifiedKind }

// Register installs this adapter's PluginConfig decoder under the
// short ("sqs") and fully-qualified ("aws.sqs") discriminators.
// Composition roots call Register exactly once per registry.
func Register(reg *ports.Registry) error {
	dec := func(raw ports.RawConfig) (ports.PluginConfig, error) {
		// Decode into a DefaultConfig()-pre-filled value so documented
		// defaults apply on the typed YAML path (wait_time_seconds 20,
		// max_messages 10) while explicit values — including an explicit
		// `wait_time_seconds: 0` or `max_messages: 0` — survive decode
		// (mapstructure only assigns keys present in the input).
		c := DefaultConfig()
		if raw != nil {
			if err := raw.Decode(&c); err != nil {
				return nil, err
			}
		}
		// Reject explicit zeros with a clear error instead of the silent
		// coercion applyDefaults would otherwise perform (Finding 12).
		// Short-polling (wait_time_seconds: 0) is intentionally
		// unsupported on the plugin surface; omit the key for the 20s
		// long-poll default. max_messages must be in [1,10].
		if c.WaitTimeSeconds == 0 {
			return nil, errors.New(
				"sqs: wait_time_seconds must be in [1,20]; short-polling (0) is " +
					"unsupported — omit the key to use the 20s long-poll default")
		}
		if c.MaxMessages == 0 {
			return nil, errors.New(
				"sqs: max_messages must be in [1,10]; omit the key to use the default of 10")
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
		reg.Register(ShortKind, dec),
		reg.Register(QualifiedKind, dec),
	)
}
