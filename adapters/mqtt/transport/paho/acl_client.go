package paho

import (
	"context"
	"fmt"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// pahoConnection is the unexported mock seam that wraps an
// autopaho.ConnectionManager and exposes a domain-shaped surface to
// the rest of the package. Port-side files (session.go,
// session_lifecycle.go, session_reconcile.go, receiver.go) drive the
// MQTT connection through this interface so they need not import the
// vendor SDK directly.
//
// Tests can substitute a fake by assigning a stub implementation to
// Session.cm — see analysis_sender_test.go::fakeCM for the canonical
// non-functional sentinel used by tests that never call any method on
// the connection.
type pahoConnection interface {
	AwaitConnection(ctx context.Context) error
	Disconnect(ctx context.Context) error
	Subscribe(ctx context.Context, subs []subscribeSpec) (reasons []byte, err error)
	Unsubscribe(ctx context.Context, topics []string) error
	PublishEnvelope(
		ctx context.Context,
		env *messaging.Envelope,
		opts SenderOptions,
		clk clock.Clock,
	) (publishResult, error)
	// Underlying exposes the raw autopaho ConnectionManager. It exists
	// solely so the public Session.ConnectionManager() accessor (kept
	// for backwards-compatible test access) can return the SDK pointer.
	// New code MUST NOT call this — use the domain-shaped methods
	// above.
	Underlying() *autopaho.ConnectionManager
}

// subscribeSpec is the domain-shaped equivalent of paho.SubscribeOptions
// used at the package boundary so reconcile and SUBACK handling remain
// SDK-free in port-side files.
type subscribeSpec struct {
	Topic string
	QoS   byte
}

// publishResult is the SDK-free view of a paho PUBACK / PUBREC. Only
// the reason code is consumed by the port side; richer fields can be
// added if future logic requires them.
type publishResult struct {
	ReasonCode byte
}

// pahoConn is the production pahoConnection backed by a real
// autopaho.ConnectionManager.
type pahoConn struct {
	cm *autopaho.ConnectionManager
}

// newPahoConn wraps a live autopaho.ConnectionManager so it can be
// stored in Session.cm (typed pahoConnection).
func newPahoConn(cm *autopaho.ConnectionManager) *pahoConn {
	return &pahoConn{cm: cm}
}

// AwaitConnection blocks until the underlying ConnectionManager
// reports a successful initial connection, ctx is cancelled, or the
// connection is permanently abandoned.
func (c *pahoConn) AwaitConnection(ctx context.Context) error {
	if err := c.cm.AwaitConnection(ctx); err != nil {
		return fmt.Errorf("paho: await connection: %w", err)
	}
	return nil
}

// Disconnect requests a clean shutdown of the underlying CM.
func (c *pahoConn) Disconnect(ctx context.Context) error {
	if err := c.cm.Disconnect(ctx); err != nil {
		return fmt.Errorf("paho: disconnect: %w", err)
	}
	return nil
}

// Subscribe issues a SUBSCRIBE for the given topic / QoS pairs and
// returns the per-topic reason codes. Reason classification stays in
// the port-side reconcile loop.
func (c *pahoConn) Subscribe(ctx context.Context, subs []subscribeSpec) ([]byte, error) {
	opts := make([]pahov5.SubscribeOptions, len(subs))
	for i, s := range subs {
		opts[i] = pahov5.SubscribeOptions{Topic: s.Topic, QoS: s.QoS}
	}
	sa, err := c.cm.Subscribe(ctx, &pahov5.Subscribe{Subscriptions: opts})
	if err != nil {
		return nil, fmt.Errorf("paho: subscribe: %w", err)
	}
	if sa == nil {
		return nil, nil
	}
	return sa.Reasons, nil
}

// Unsubscribe issues an UNSUBSCRIBE for the given topics.
func (c *pahoConn) Unsubscribe(ctx context.Context, topics []string) error {
	if _, err := c.cm.Unsubscribe(ctx, &pahov5.Unsubscribe{Topics: topics}); err != nil {
		return fmt.Errorf("paho: unsubscribe: %w", err)
	}
	return nil
}

// PublishEnvelope serialises the given messaging.Envelope into a paho
// Publish via PublishFromEnvelope and forwards it to the broker. The
// PUBACK / PUBREC reason code is returned in publishResult so the port
// side can map it via MapPublishReasonCode without importing the SDK.
func (c *pahoConn) PublishEnvelope(
	ctx context.Context,
	env *messaging.Envelope,
	opts SenderOptions,
	clk clock.Clock,
) (publishResult, error) {
	pub := PublishFromEnvelope(env, opts, clk)
	resp, err := c.cm.Publish(ctx, pub)
	if err != nil {
		return publishResult{}, fmt.Errorf("paho: publish: %w", err)
	}
	if resp == nil {
		return publishResult{}, nil
	}
	return publishResult{ReasonCode: resp.ReasonCode}, nil
}

// Underlying returns the raw autopaho.ConnectionManager. Used only by
// the legacy ConnectionManager() accessor and tests.
func (c *pahoConn) Underlying() *autopaho.ConnectionManager {
	return c.cm
}
