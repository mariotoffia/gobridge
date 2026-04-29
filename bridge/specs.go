package bridge

import (
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// sessionSpecFrom converts a ports.SessionDef to a ports.SessionSpec.
// The conversion is purely structural: plugin-specific shape inside
// Options is left as a generic map and parsed by the adapter.
func sessionSpecFrom(def ports.SessionDef) ports.SessionSpec {
	return ports.SessionSpec{
		ID:          def.ID,
		Transport:   def.Transport,
		SessionMode: domain.SessionMode(def.SessionMode),
		Options:     def.Options,
	}
}

// receiverSpecFrom converts a ports.ReceiverDef to a ports.ReceiverSpec.
// SubscriptionDef entries are mapped to domain.SubscriptionPlan values.
func receiverSpecFrom(def ports.ReceiverDef) ports.ReceiverSpec {
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

// senderSpecFrom converts a ports.SenderDef to a ports.SenderSpec.
func senderSpecFrom(def ports.SenderDef) ports.SenderSpec {
	return ports.SenderSpec{
		ID:        def.ID,
		SessionID: def.SessionID,
		Options:   def.Options,
	}
}

// storeSpecFrom converts a ports.StoreConfig to a ports.StoreSpec.
// The two types have identical shape (Type + Options); the explicit
// conversion makes the boundary visible in code.
func storeSpecFrom(cfg ports.StoreConfig) ports.StoreSpec {
	return ports.StoreSpec(cfg)
}
