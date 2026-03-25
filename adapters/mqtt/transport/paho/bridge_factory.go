package paho

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ bridge.TransportFactory = (*BridgeFactory)(nil)

type BridgeFactory struct {
	inner Factory
}

func NewBridgeFactory(logger *slog.Logger) *BridgeFactory {
	return &BridgeFactory{
		inner: Factory{Logger: logger},
	}
}

func (bf *BridgeFactory) NewSession(ctx context.Context, def config.SessionDef) (ports.Session, error) {
	return bf.inner.NewSession(ctx, ports.SessionSpec{
		ID:          def.ID,
		Transport:   def.Transport,
		SessionMode: domain.SessionMode(def.SessionMode),
		Options:     def.Options,
	})
}

func (bf *BridgeFactory) NewReceiver(ctx context.Context, def config.ReceiverDef, session ports.Session) (ports.Receiver, error) {
	subs := make([]domain.SubscriptionPlan, len(def.Topics))
	for i, t := range def.Topics {
		subs[i] = domain.SubscriptionPlan{
			Topic:   t.Topic,
			QoS:     t.QoS,
			Options: t.Options,
		}
	}

	return bf.inner.NewReceiver(ctx, ports.ReceiverSpec{
		ID:            def.ID,
		SessionID:     def.SessionID,
		Subscriptions: subs,
		Options:       def.Options,
	}, session)
}

func (bf *BridgeFactory) NewSender(ctx context.Context, def config.SenderDef, session ports.Session) (ports.Sender, error) {
	return bf.inner.NewSender(ctx, ports.SenderSpec{
		ID:        def.ID,
		SessionID: def.SessionID,
		Options:   def.Options,
	}, session)
}

func (bf *BridgeFactory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapStatefulSession,
		ports.CapExclusiveIdentity,
	}
}
