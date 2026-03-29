package servicebus

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.ReceiverFactory   = (*ReceiverFactory)(nil)
	_ ports.SenderFactory     = (*SenderFactory)(nil)
	_ bridge.TransportFactory = (*BridgeFactory)(nil)
)

type ReceiverFactory struct {
	logger *slog.Logger
}

func NewReceiverFactory(logger *slog.Logger) *ReceiverFactory {
	return &ReceiverFactory{logger: logger}
}

func (f *ReceiverFactory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	cfg := ReceiverConfigFromOptions(spec.Options)
	return NewReceiver(cfg, f.logger)
}

type SenderFactory struct {
	logger *slog.Logger
}

func NewSenderFactory(logger *slog.Logger) *SenderFactory {
	return &SenderFactory{logger: logger}
}

func (f *SenderFactory) NewSender(_ context.Context, spec ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	cfg := SenderConfigFromOptions(spec.Options)
	cfg.Logger = f.logger
	return NewSender(cfg)
}

type BridgeFactory struct {
	recvFactory *ReceiverFactory
	sendFactory *SenderFactory
}

func NewBridgeFactory(logger *slog.Logger) *BridgeFactory {
	return &BridgeFactory{
		recvFactory: NewReceiverFactory(logger),
		sendFactory: NewSenderFactory(logger),
	}
}

func (f *BridgeFactory) NewSession(_ context.Context, _ config.SessionDef) (ports.Session, error) {
	return nil, nil
}

func (f *BridgeFactory) NewReceiver(ctx context.Context, def config.ReceiverDef, session ports.Session) (ports.Receiver, error) {
	spec := ports.ReceiverSpec{
		ID:        def.ID,
		SessionID: def.SessionID,
		Options:   def.Options,
	}
	return f.recvFactory.NewReceiver(ctx, spec, session)
}

func (f *BridgeFactory) NewSender(ctx context.Context, def config.SenderDef, session ports.Session) (ports.Sender, error) {
	spec := ports.SenderSpec{
		ID:        def.ID,
		SessionID: def.SessionID,
		Options:   def.Options,
	}
	return f.sendFactory.NewSender(ctx, spec, session)
}

func (f *BridgeFactory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapVisibilityExtension,
	}
}
