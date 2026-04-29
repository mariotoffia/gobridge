package bridge

import (
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// sessionSpecFrom converts a config.SessionDef to a ports.SessionSpec.
// The conversion is purely structural: plugin-specific shape inside
// Options is left as a generic map and parsed by the adapter.
func sessionSpecFrom(def config.SessionDef) ports.SessionSpec {
	return ports.SessionSpec{
		ID:          def.ID,
		Transport:   def.Transport,
		SessionMode: domain.SessionMode(def.SessionMode),
		Options:     def.Options,
	}
}

// receiverSpecFrom converts a config.ReceiverDef to a ports.ReceiverSpec.
// SubscriptionDef entries are mapped to domain.SubscriptionPlan values.
func receiverSpecFrom(def config.ReceiverDef) ports.ReceiverSpec {
	var subs []domain.SubscriptionPlan
	if len(def.Topics) > 0 {
		subs = make([]domain.SubscriptionPlan, len(def.Topics))
		for i, t := range def.Topics {
			subs[i] = domain.SubscriptionPlan{
				Topic:   t.Topic,
				QoS:     t.QoS,
				Options: t.Options,
			}
		}
	}
	return ports.ReceiverSpec{
		ID:            def.ID,
		SessionID:     def.SessionID,
		Subscriptions: subs,
		Options:       def.Options,
	}
}

// senderSpecFrom converts a config.SenderDef to a ports.SenderSpec.
func senderSpecFrom(def config.SenderDef) ports.SenderSpec {
	return ports.SenderSpec{
		ID:        def.ID,
		SessionID: def.SessionID,
		Options:   def.Options,
	}
}

// storeSpecFrom converts a config.StoreConfig to a ports.StoreSpec.
func storeSpecFrom(cfg config.StoreConfig) ports.StoreSpec {
	return ports.StoreSpec{
		Type:    cfg.Type,
		Options: cfg.Options,
	}
}
