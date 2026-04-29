package ports

import (
	"context"
	"net/http"
	"time"

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

// TransportFactory is the port-level contract a transport adapter
// implements so it can be registered with bridge.Builder. It composes
// the per-role factories with a Capabilities query used by the runtime
// validator.
//
// Stateless transports (e.g. SQS, HTTP) return (nil, nil) from
// NewSession. The session parameter passed to NewReceiver/NewSender
// may be nil for stateless transports.
type TransportFactory interface {
	SessionFactory
	ReceiverFactory
	SenderFactory

	// Capabilities returns the transport capabilities relevant for
	// startup validation (e.g. visibility extension, stateful session).
	Capabilities() []Capability
}

// VisibilityTimeoutProvider is an optional interface that
// TransportFactory implementations may satisfy to declare the source
// visibility timeout. The runtime validator uses this value to check
// that SendTimeout does not exceed half the visibility window, which
// would cause duplicate processing.
type VisibilityTimeoutProvider interface {
	VisibilityTimeout() time.Duration
}

// HTTPMountable is implemented by transport factories that expose HTTP
// handlers. The composition root extracts the handler and mounts it on
// an HTTP server alongside the admin/monitor APIs.
type HTTPMountable interface {
	Handler() http.Handler
	PathPrefix() string
}
