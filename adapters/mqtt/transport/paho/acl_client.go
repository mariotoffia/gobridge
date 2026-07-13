package paho

import (
	"context"
	"errors"
	"fmt"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
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
		topic string,
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
	// NoLocal requests that the broker NOT deliver a message back to the
	// connection that published it (MQTT5 §3.8.3.1). It breaks the
	// MQTT->MQTT self-delivery loop: a session that subscribes and publishes on
	// filters that overlap would otherwise receive its OWN forwarded publishes
	// and re-forward them, an unbounded self-amplification (Scenario 01). It is
	// opt-in per session (SessionOptions.NoLocal / `no_local`, default off) and
	// MUST stay false for a shared subscription ($share), where NoLocal is a
	// Protocol Error the broker rejects with a DISCONNECT. Cross-bridge delivery
	// is unaffected: NoLocal is per-connection, and distinct bridges use
	// distinct client_ids.
	NoLocal bool
	// RetainHandling controls WHEN the broker sends retained messages for the
	// filter at subscribe time (MQTT5 §3.8.3.1):
	//   0 — always send retained on subscribe;
	//   1 — send retained only if the subscription did not already exist;
	//   2 — never send retained on subscribe.
	// Ephemeral sessions use 0 (each connect is a brand-new subscription, so
	// retained state must be (re)hydrated). Persistent/exclusive sessions use 1:
	// on the FIRST subscribe the subscription does not exist yet so retained
	// state still arrives, but on every reconnect that RESUMES the session the
	// subscription already exists broker-side, so the broker suppresses a full
	// retained replay — avoiding the reconnect-storm where each network blip
	// re-delivers every retained message per filter and direct routes
	// re-process retained state (A-7).
	RetainHandling byte
}

// publishResult is the SDK-free view of a paho PUBACK / PUBREC. Only
// the reason code is consumed by the port side; richer fields can be
// added if future logic requires them.
type publishResult struct {
	ReasonCode byte
}

// connackReasonCode extracts the CONNACK reason code from a rejected autopaho
// connect error. A server-side CONNECT denial surfaces as
// *autopaho.ConnackError whose ReasonCode carries the auth verdict (0x86 bad
// user/pass, 0x87 not authorized). Returning the plain byte keeps the vendor
// SDK boundary inside this ACL file: the port side (mapConnectError) maps the
// code via MapDisconnectReasonCode without importing the SDK. ok is false when
// err is not a CONNACK denial (e.g. a network dial failure), in which case the
// caller falls back to the generic MapError.
func connackReasonCode(err error) (code byte, ok bool) {
	var ce *autopaho.ConnackError
	if errors.As(err, &ce) {
		return ce.ReasonCode, true
	}
	return 0, false
}

// pahoConn is the production pahoConnection backed by a real
// autopaho.ConnectionManager.
type pahoConn struct {
	cm *autopaho.ConnectionManager
	// metrics is the session's exporter, threaded so PublishEnvelope can
	// count egress header drops (MetricMQTTNonStringHeaderDropped) on the
	// Sender path — the same counting the Sender used to do when it called
	// PublishFromEnvelope directly (F-2). May be nil; PublishFromEnvelope
	// tolerates a nil exporter (drop applied, uncounted).
	metrics ports.MetricsExporter
}

// newPahoConn wraps a live autopaho.ConnectionManager so it can be
// stored in Session.cm (typed pahoConnection). metrics is the session's
// exporter, used only by PublishEnvelope for egress drop counting.
func newPahoConn(cm *autopaho.ConnectionManager, metrics ports.MetricsExporter) *pahoConn {
	return &pahoConn{cm: cm, metrics: metrics}
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
		opts[i] = pahov5.SubscribeOptions{
			Topic:          s.Topic,
			QoS:            s.QoS,
			NoLocal:        s.NoLocal,
			RetainHandling: s.RetainHandling,
		}
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
// publish topic is supplied explicitly by the caller (it is the
// transport-level destination, distinct from env.Subject()). The
// PUBACK / PUBREC reason code is returned in publishResult so the port
// side can map it via MapPublishReasonCode without importing the SDK.
func (c *pahoConn) PublishEnvelope(
	ctx context.Context,
	env *messaging.Envelope,
	topic string,
	opts SenderOptions,
	clk clock.Clock,
) (publishResult, error) {
	pub := PublishFromEnvelope(env, topic, opts, clk, c.metrics)
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
