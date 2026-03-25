package bridge

import (
	"context"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

// TransportFactory creates transport-specific sessions, receivers, and senders
// from declarative configuration definitions. A transport that does not
// support sessions (e.g. SQS) should return (nil, nil) from NewSession.
type TransportFactory interface {
	// NewSession creates a session from the given definition.
	// Return (nil, nil) if the transport is stateless.
	NewSession(ctx context.Context, def config.SessionDef) (ports.Session, error)

	// NewReceiver creates a receiver bound to the optional session.
	NewReceiver(ctx context.Context, def config.ReceiverDef, session ports.Session) (ports.Receiver, error)

	// NewSender creates a sender bound to the optional session.
	NewSender(ctx context.Context, def config.SenderDef, session ports.Session) (ports.Sender, error)

	// Capabilities returns the transport capabilities relevant for
	// startup validation (e.g. visibility extension, stateful session).
	Capabilities() []ports.Capability
}

// StoreFactory creates backing store instances from declarative
// configuration. Implementations wrap a specific store technology
// (e.g. DynamoDB, memory, SQLite).
type StoreFactory interface {
	// NewLeaseStore creates a lease store from the given config.
	// Return (nil, nil) if the factory does not handle this store type.
	NewLeaseStore(ctx context.Context, cfg config.StoreConfig) (ports.LeaseStore, error)

	// NewOutboxStore creates an outbox store from the given config.
	// Return (nil, nil) if the factory does not handle this store type.
	NewOutboxStore(ctx context.Context, cfg config.StoreConfig) (ports.OutboxStore, error)

	// NewDLQStore creates a DLQ store from the given config.
	// Return (nil, nil) if the factory does not handle this store type.
	NewDLQStore(ctx context.Context, cfg config.StoreConfig) (ports.DLQStore, error)
}
