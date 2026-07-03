package bridge

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capTransportFactory is a fakeTransportFactory whose Capabilities() are
// configurable, letting a test advertise (or omit) ports.CapPlanDrivenSubscriptions
// without importing a real transport adapter (forbidden to the bridge package).
type capTransportFactory struct {
	fakeTransportFactory
	caps []ports.Capability
}

func (f *capTransportFactory) Capabilities() []ports.Capability { return f.caps }

// TestBuilder_PlanDrivenReceiverWithoutSessionManager_ADVP4FU1 is the focused
// guard for ADV-P4-FU1: a receiver whose session_id resolves to NO session
// manager (no route names it as a primary session, no binding targets it) is
// silently inert because nothing ever reconciles its subscriptions.
//
// The builder must FAIL the build for a PLAN-DRIVEN source (mqtt/amqp091 — those
// advertising CapPlanDrivenSubscriptions), whose subscriptions are established
// only by the session manager reconciling the plan, and must BUILD FINE for a
// SELF-ESTABLISHING source (amqp10) that attaches links independently of the
// plan — the exact false-positive that blocked a validate-layer fix.
func TestBuilder_PlanDrivenReceiverWithoutSessionManager_ADVP4FU1(t *testing.T) {
	// makeCfg builds a route whose source receiver subscribes on s-src but whose
	// route has NO session block and forwards to a session-less sink sender, so
	// s-src is never wired with a session manager.
	makeCfg := func(sourceTransport string) *ports.BridgeConfig {
		return &ports.BridgeConfig{
			Bridge: ports.BridgeSettings{ID: "b-fu1"},
			Sessions: []ports.SessionDef{
				{ID: "s-src", Transport: sourceTransport, SessionMode: "exclusive"},
			},
			Receivers: []ports.ReceiverDef{
				{ID: "rx", Transport: sourceTransport, SessionID: "s-src", Topics: []ports.SubscriptionDef{
					{Topic: "topic/in", QoS: 1},
				}},
			},
			Senders: []ports.SenderDef{
				{ID: "tx", Transport: "sink"}, // session-less egress
			},
			Bindings: []ports.BindingDef{
				{ID: "b1", SenderID: "tx", Address: "out/addr"},
			},
			Routes: []ports.RouteDef{
				{
					ID:           "r1",
					ReceiverID:   "rx",
					DeliveryMode: "direct_hold",
					Bindings:     []string{"b1"},
					// drop policies keep the route valid under the build-time
					// ValidateRoutes call (Finding 5 / C2) so this test isolates
					// the ADV-P4-FU1 unmanaged-session check, not DLQ policy.
					Policy: ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
					// No Session block: s-src is neither a route-primary session
					// nor a binding session, so it gets no manager.
				},
			},
		}
	}

	t.Run("plan-driven source (mqtt-like) fails the build naming receiver, session and transport", func(t *testing.T) {
		planDriven := &capTransportFactory{caps: []ports.Capability{
			ports.CapStatefulSession, ports.CapPlanDrivenSubscriptions,
		}}
		_, err := NewBuilder(makeCfg("mqtt")).
			RegisterTransportFactory("mqtt", planDriven).
			RegisterTransportFactory("sink", &fakeTransportFactory{}).
			Build(context.Background())

		require.Error(t, err, "a plan-driven receiver on an unmanaged session must not build silently inert")
		assert.Contains(t, err.Error(), `receiver "rx"`)
		assert.Contains(t, err.Error(), `session "s-src"`)
		assert.Contains(t, err.Error(), "mqtt")
	})

	t.Run("self-establishing source (amqp10-like) builds fine", func(t *testing.T) {
		selfEstablishing := &capTransportFactory{caps: []ports.Capability{
			ports.CapStatefulSession, // deliberately NO CapPlanDrivenSubscriptions
			// direct_hold requires the source to support visibility extension;
			// amqp10 sources hold the message for the delivery window, so this
			// is realistic and keeps ValidateRoutes (Finding 5) focused on the
			// ADV-P4-FU1 behaviour under test rather than an unrelated mode error.
			ports.CapVisibilityExtension,
		}}
		rt, err := NewBuilder(makeCfg("amqp10")).
			RegisterTransportFactory("amqp10", selfEstablishing).
			RegisterTransportFactory("sink", &fakeTransportFactory{}).
			Build(context.Background())

		require.NoError(t, err, "a self-establishing receiver does not need a plan-reconciling manager")
		require.NotNil(t, rt)
	})
}
