package amqp10

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ bridge.TransportFactory = (*BridgeFactory)(nil)

// BridgeFactory implements bridge.TransportFactory for AMQP 1.0 transports.
type BridgeFactory struct {
	inner Factory
}

// NewBridgeFactory creates a BridgeFactory for AMQP 1.0.
func NewBridgeFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *BridgeFactory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &BridgeFactory{
		inner: Factory{Logger: logger, Metrics: m},
	}
}

// NewSession creates an AMQP 1.0 Session from the given definition.
func (bf *BridgeFactory) NewSession(ctx context.Context, def config.SessionDef) (ports.Session, error) {
	return bf.inner.NewSession(ctx, ports.SessionSpec{
		ID:          def.ID,
		Transport:   def.Transport,
		SessionMode: domain.SessionMode(def.SessionMode),
		Options:     def.Options,
	})
}

// NewReceiver creates an AMQP 1.0 Receiver from the given definition.
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

// NewSender creates an AMQP 1.0 Sender from the given definition.
func (bf *BridgeFactory) NewSender(ctx context.Context, def config.SenderDef, session ports.Session) (ports.Sender, error) {
	return bf.inner.NewSender(ctx, ports.SenderSpec{
		ID:        def.ID,
		SessionID: def.SessionID,
		Options:   def.Options,
	}, session)
}

// Capabilities returns the transport capabilities for AMQP 1.0.
func (bf *BridgeFactory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapStatefulSession,
	}
}
