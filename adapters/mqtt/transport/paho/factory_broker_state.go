package paho

import (
	"fmt"

	"github.com/mariotoffia/gobridge/ports"
)

// BrokerStateKeys implements ports.BrokerStateKeyer (ADR 0024). A persistent or
// exclusive session holds one key, its Config.BrokerStateKey; an ephemeral
// session holds none. MQTT receivers add nothing: the broker keeps their
// subscriptions on the session.
func (f *Factory) BrokerStateKeys(session ports.SessionSpec, _ []ports.ReceiverSpec) ([]string, error) {
	cfg, err := configFromSpec(session.Config)
	if err != nil {
		return nil, fmt.Errorf("mqtt session %q: %w", session.ID, err)
	}
	key, err := cfg.BrokerStateKey(session.SessionMode)
	if err != nil {
		return nil, fmt.Errorf("mqtt session %q: broker state key: %w", session.ID, err)
	}
	if key == "" {
		return nil, nil
	}
	return []string{key}, nil
}

var _ ports.BrokerStateKeyer = (*Factory)(nil)
