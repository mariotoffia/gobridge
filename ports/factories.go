package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain"
)

// SessionSpec holds transport connection identity and remote session
// behavior configuration. Transport-specific settings go in Options.
type SessionSpec struct {
	ID          string
	Transport   string
	SessionMode domain.SessionMode
	Options     map[string]any
}

// ReceiverSpec holds ingress behavior configuration.
// Transport-specific settings go in Options.
type ReceiverSpec struct {
	ID            string
	SessionID     string
	Subscriptions []domain.SubscriptionPlan
	Options       map[string]any
}

// SenderSpec holds egress behavior configuration.
// Transport-specific settings go in Options.
type SenderSpec struct {
	ID        string
	SessionID string
	Options   map[string]any
}

// SessionFactory creates Session instances for stateful transports.
type SessionFactory interface {
	NewSession(ctx context.Context, spec SessionSpec) (Session, error)
}

// ReceiverFactory creates Receiver instances.
// The session parameter may be nil for stateless transports.
type ReceiverFactory interface {
	NewReceiver(ctx context.Context, spec ReceiverSpec, session Session) (Receiver, error)
}

// SenderFactory creates Sender instances.
// The session parameter may be nil for stateless transports.
type SenderFactory interface {
	NewSender(ctx context.Context, spec SenderSpec, session Session) (Sender, error)
}
