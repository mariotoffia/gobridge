package sqs

import (
	"context"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ bridge.TransportFactory          = (*BridgeFactory)(nil)
	_ bridge.VisibilityTimeoutProvider = (*BridgeFactory)(nil)
)

// BridgeFactory adapts the SQS ReceiverFactory and SenderFactory to the
// bridge.TransportFactory interface used by bridge.Builder.
type BridgeFactory struct {
	recvFactory *ReceiverFactory
	sendFactory *SenderFactory
}

// NewBridgeFactory returns a BridgeFactory that creates SQS receivers
// and senders from declarative config definitions.
func NewBridgeFactory(logger *slog.Logger) *BridgeFactory {
	return &BridgeFactory{
		recvFactory: NewReceiverFactory(logger),
		sendFactory: NewSenderFactory(logger),
	}
}

// NewSession returns (nil, nil) because SQS is a stateless transport.
func (f *BridgeFactory) NewSession(_ context.Context, _ config.SessionDef) (ports.Session, error) {
	return nil, nil
}

// NewReceiver converts a config.ReceiverDef to a ports.ReceiverSpec and
// delegates to the wrapped ReceiverFactory.
func (f *BridgeFactory) NewReceiver(ctx context.Context, def config.ReceiverDef, session ports.Session) (ports.Receiver, error) {
	spec := ports.ReceiverSpec{
		ID:        def.ID,
		SessionID: def.SessionID,
		Options:   def.Options,
	}
	return f.recvFactory.NewReceiver(ctx, spec, session)
}

// NewSender converts a config.SenderDef to a ports.SenderSpec and
// delegates to the wrapped SenderFactory.
func (f *BridgeFactory) NewSender(ctx context.Context, def config.SenderDef, session ports.Session) (ports.Sender, error) {
	spec := ports.SenderSpec{
		ID:        def.ID,
		SessionID: def.SessionID,
		Options:   def.Options,
	}
	return f.sendFactory.NewSender(ctx, spec, session)
}

// Capabilities returns the transport capabilities of SQS: visibility
// extension (for delivery lock renewal) and source redelivery (SQS
// automatically redelivers un-deleted messages).
func (f *BridgeFactory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapVisibilityExtension,
		ports.CapSourceRedelivery,
	}
}

// VisibilityTimeout returns the default SQS visibility timeout (30s).
// The runtime validator uses this to check that SendTimeout does not
// exceed half the visibility window.
func (f *BridgeFactory) VisibilityTimeout() time.Duration {
	return 30 * time.Second
}
