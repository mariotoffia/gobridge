package amqp091

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ bridge.TransportFactory = (*BridgeFactory)(nil)

// BridgeFactory implements bridge.TransportFactory for AMQP 0-9-1
// (RabbitMQ), creating sessions, receivers, and senders from
// declarative configuration definitions.
type BridgeFactory struct {
	sessionFactory  *Factory
	receiverFactory *ReceiverFactory
	senderFactory   *SenderFactory
}

// NewBridgeFactory creates a BridgeFactory for AMQP 0-9-1.
func NewBridgeFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *BridgeFactory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &BridgeFactory{
		sessionFactory:  &Factory{Logger: logger, Metrics: m},
		receiverFactory: NewReceiverFactory(logger),
		senderFactory:   NewSenderFactory(logger),
	}
}

// NewSession creates an AMQP 0-9-1 Session from the given definition.
func (bf *BridgeFactory) NewSession(ctx context.Context, def config.SessionDef) (ports.Session, error) {
	return bf.sessionFactory.NewSession(ctx, ports.SessionSpec{
		ID:          def.ID,
		Transport:   def.Transport,
		SessionMode: domain.SessionMode(def.SessionMode),
		Options:     def.Options,
	})
}

// NewReceiver creates an AMQP 0-9-1 Receiver from the given definition.
func (bf *BridgeFactory) NewReceiver(ctx context.Context, def config.ReceiverDef, session ports.Session) (ports.Receiver, error) {
	subs := make([]domain.SubscriptionPlan, len(def.Topics))
	for i, t := range def.Topics {
		subs[i] = domain.SubscriptionPlan{
			Topic:   t.Topic,
			QoS:     t.QoS,
			Options: t.Options,
		}
	}

	return bf.receiverFactory.NewReceiver(ctx, ports.ReceiverSpec{
		ID:            def.ID,
		SessionID:     def.SessionID,
		Subscriptions: subs,
		Options:       def.Options,
	}, session)
}

// NewSender creates an AMQP 0-9-1 Sender from the given definition.
func (bf *BridgeFactory) NewSender(ctx context.Context, def config.SenderDef, session ports.Session) (ports.Sender, error) {
	return bf.senderFactory.NewSender(ctx, ports.SenderSpec{
		ID:        def.ID,
		SessionID: def.SessionID,
		Options:   def.Options,
	}, session)
}

// Capabilities returns the transport capabilities for AMQP 0-9-1.
// RabbitMQ supports stateful sessions and source-level redelivery
// via nack+requeue.
func (bf *BridgeFactory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapStatefulSession,
		ports.CapSourceRedelivery,
	}
}
