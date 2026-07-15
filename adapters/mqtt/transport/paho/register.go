package paho

import (
	"errors"

	"github.com/mariotoffia/gobridge/ports"
)

// Register installs this adapter's PluginConfig decoder under the
// short ("mqtt") and fully-qualified ("mqtt.paho") discriminators.
//
// The decoder decodes into a DefaultConfig()-pre-filled value. Ordinary
// defaults (keep_alive, timeouts, sender QoS, …) materialize immediately, while
// the three ingress-memory fields remain zero/unset until deployment preflight
// or NewSession normalization. Keeping those zeros through YAML clone/parse is
// what distinguishes omitted defaults from explicit operator values for the AWS
// multi-session memory profile.
func Register(reg *ports.Registry) error {
	dec := func(raw ports.RawConfig) (ports.PluginConfig, error) {
		c := DefaultConfig()
		if raw != nil {
			if err := raw.Decode(&c); err != nil {
				return nil, err
			}
			var decoded map[string]any
			if err := raw.Decode(&decoded); err != nil {
				return nil, err
			}
			if session, ok := decoded["session"].(map[string]any); ok {
				_, receiveMaximumSet := session["receive_maximum"]
				c.Session.receiveMaximumExplicit =
					receiveMaximumSet && c.Session.ReceiveMaximum != 0
				_, ingressBudgetSet := session["ingress_memory_budget_bytes"]
				c.Session.ingressMemoryBudgetExplicit =
					ingressBudgetSet && c.Session.IngressMemoryBudgetBytes != 0
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
