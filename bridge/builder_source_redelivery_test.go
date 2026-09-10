package bridge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// Threading a per-ROUTE redelivery verdict from the transport to the validator.
//
// Whether the source hands a message back after a crash is not always a property
// of the transport: on a plan-driven one it depends on the session the receiver
// binds to and on the QoS of the subscriptions the route runs with, neither of
// which the receiver's own options carry. The builder is the only place that has
// all three in scope, so it is the builder that asks — and the refusal has to
// arrive intact, because "the session throws itself away" and "the subscription
// is at-most-once" are the same verdict with different fixes.

// redeliveryVerdictConfig is a receiver plugin config that answers the
// redelivery question the way a real plan-driven transport does: from the
// session it is handed and the subscriptions it is handed.
type redeliveryVerdictConfig struct {
	sawSession string
	sawTopics  []string
	refuse     string
}

func (c *redeliveryVerdictConfig) Kind() string    { return "sqs" }
func (c *redeliveryVerdictConfig) Validate() error { return nil }

func (c *redeliveryVerdictConfig) SourceRedeliversUnsettled(
	session ports.SessionSpec,
	subscriptions []connectivity.SubscriptionPlan,
) (bool, string) {
	c.sawSession = session.ID
	c.sawTopics = nil
	for _, subscription := range subscriptions {
		c.sawTopics = append(c.sawTopics, subscription.Topic)
	}
	if c.refuse != "" {
		return false, c.refuse
	}
	return true, ""
}

var _ ports.SourceRedeliveryConfig = (*redeliveryVerdictConfig)(nil)

// directHoldConfigWithVerdict is a one-route direct_hold config whose receiver
// answers the redelivery question itself.
func directHoldConfigWithVerdict(verdict *redeliveryVerdictConfig) *ports.BridgeConfig {
	receiver := ports.ReceiverDef{
		ID: "src-rx", Transport: "sqs", SessionID: "src-session",
		Topics: []ports.SubscriptionDef{{Topic: "sensors/#", QoS: 1}},
	}
	receiver.SetDecoded(verdict, nil)
	return &ports.BridgeConfig{
		Bridge:   ports.BridgeSettings{ID: "test-bridge"},
		Sessions: []ports.SessionDef{{ID: "src-session", Transport: "sqs"}},
		Receivers: []ports.ReceiverDef{
			receiver,
		},
		Senders:  []ports.SenderDef{{ID: "sink-tx", Transport: "sink"}},
		Bindings: []ports.BindingDef{{ID: "b1", SenderID: "sink-tx", Address: "queue/out"}},
		Routes: []ports.RouteDef{{
			ID: "r1", ReceiverID: "src-rx", DeliveryMode: "direct_hold",
			Bindings: []string{"b1"},
			Policy:   ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
		}},
	}
}

// noCapabilityTransport declares nothing, so the route's admissibility rests
// entirely on the per-route verdict — which is the case this exists to cover.
type noCapabilityTransport struct{ fakeTransportFactory }

func (f *noCapabilityTransport) Capabilities() []ports.Capability { return nil }

func buildWithVerdict(t *testing.T, verdict *redeliveryVerdictConfig) error {
	t.Helper()
	rt, err := NewBuilder(directHoldConfigWithVerdict(verdict)).
		RegisterTransportFactory("sqs", &noCapabilityTransport{}).
		RegisterTransportFactory("sink", &noCapabilityTransport{}).
		Build(context.Background())
	if err != nil {
		// Route validation runs at Build; a refused route never reaches Start.
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := rt.Start(ctx)
	if startErr == nil {
		_ = rt.Stop(context.Background())
	}
	return startErr
}

// TestBuilder_PerRouteRedeliveryAdmitsDirectHold pins the accepting direction,
// and that the transport was asked the question with the facts it needs.
func TestBuilder_PerRouteRedeliveryAdmitsDirectHold(t *testing.T) {
	verdict := &redeliveryVerdictConfig{}

	require.NoError(t, buildWithVerdict(t, verdict))
	require.Equal(t, "src-session", verdict.sawSession,
		"the transport must be handed the INGRESS session, which is where a durable "+
			"delivery guarantee lives")
	require.Equal(t, []string{"sensors/#"}, verdict.sawTopics,
		"and the route's subscriptions, which is where the per-message guarantee lives")
}

// TestBuilder_PerRouteRedeliveryRefusalReachesTheOperator pins the refusing
// direction. A route that is turned down must say which precondition it failed:
// the count of things wrong with a config is not something an operator can act
// on, and only the transport knows which one it was.
func TestBuilder_PerRouteRedeliveryRefusalReachesTheOperator(t *testing.T) {
	verdict := &redeliveryVerdictConfig{refuse: `subscription "sensors/#" is QoS 0`}

	err := buildWithVerdict(t, verdict)
	require.Error(t, err)
	require.Contains(t, err.Error(), "direct_hold invalid: the source does not redeliver an unsettled message")
	require.Contains(t, err.Error(), `subscription "sensors/#" is QoS 0`)
}
