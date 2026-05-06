package bridge

import (
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// sessionSpecFrom converts a ports.SessionDef to a ports.SessionSpec.
// The typed Config produced by the two-stage parser is forwarded
// verbatim; the bridge does no further decoding.
func sessionSpecFrom(def ports.SessionDef) ports.SessionSpec {
	return ports.SessionSpec{
		ID:          def.ID,
		Transport:   def.Transport,
		SessionMode: domain.SessionMode(def.SessionMode),
		Config:      def.Config,
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
				Topic:  t.Topic,
				QoS:    t.QoS,
				Config: t.Config,
			}
		}
	}
	return ports.ReceiverSpec{
		ID:            def.ID,
		SessionID:     def.SessionID,
		Subscriptions: subs,
		Config:        def.Config,
	}
}

// senderSpecFrom converts a ports.SenderDef to a ports.SenderSpec.
func senderSpecFrom(def ports.SenderDef) ports.SenderSpec {
	return ports.SenderSpec{
		ID:        def.ID,
		SessionID: def.SessionID,
		Config:    def.Config,
	}
}

// Package bridge specs.go intentionally omits a StoreSpec converter:
// post-PHASE3 the bridge passes the typed PluginConfig from
// ports.StoreConfig directly to the StoreFactory and threads outbox
// runtime tuning (stale claim duration) through ports.OutboxRuntimeOptions.
