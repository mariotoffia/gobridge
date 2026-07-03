package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// newTrustHeadersRunner builds a two-binding direct_hold route whose resolver
// always selects bind-a, so a test can prove that an inbound
// x-bridge.route-override cannot steer dispatch to bind-b regardless of the
// TrustBridgeHeaders posture.
func newTrustHeadersRunner(trust bool, senderA, senderB *FakeSender) (*FakeReceiver, *route.RouteRunner) {
	bindings := []routing.DestinationBinding{
		{ID: "bind-a", Address: "topic-a"},
		{ID: "bind-b", Address: "topic-b"},
	}
	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{
		{BindingID: "bind-a"}, // no conditions = always match → default to bind-a
	})
	resolver, _ := runtime.NewRuleResolver(bindings, rules, "")
	receiver := NewFakeReceiver()
	cfg := route.RouteRunnerConfig{
		RouteID: "trust-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			TrustBridgeHeaders: trust,
		}.WithDefaults(),
		Receiver: receiver,
		Sender:   senderA,
		Senders:  map[string]ports.Sender{"bind-a": senderA, "bind-b": senderB},
		DLQ:      dlq.New(NewFakeDLQStore()),
		Resolver: resolver,
		Bindings: bindings,
	}
	return receiver, route.NewRouteRunnerFromConfig(cfg)
}

// TestTrustBridgeHeaders_Enabled_PreservesPropagatedHeaders proves that with
// TrustBridgeHeaders=true a delivery from a trusted upstream bridge keeps its
// BRIDGE-TO-BRIDGE PROPAGATED headers (correlation-id, tenant-id) across the
// hop instead of having them stripped and the correlation ID regenerated — yet
// the INTERNAL-ONLY x-bridge.route-override is STILL stripped so an inbound
// header can never steer routing (security invariant).
func TestTrustBridgeHeaders_Enabled_PreservesPropagatedHeaders(t *testing.T) {
	senderA := NewFakeSender()
	senderB := NewFakeSender()
	receiver, runner := newTrustHeadersRunner(true, senderA, senderB)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID:      "trust-msg",
		Subject: "test",
		Headers: map[string]any{
			messaging.HeaderCorrelationID: "abc",
			messaging.HeaderTenantID:      "t1",
			messaging.HeaderRouteOverride: "bind-b",
		},
	})
	del := NewFakeDelivery(env)
	require.NoError(t, receiver.Emit(ctx, del))
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	// Security invariant: the inbound route-override must NOT steer dispatch.
	require.Equal(t, 1, senderA.SentCount(), "must dispatch to the resolver's binding")
	require.Equal(t, 0, senderB.SentCount(), "inbound x-bridge.route-override must NOT steer routing")

	sent := senderA.GetSent()
	require.Len(t, sent, 1)
	corr, _ := messaging.GetHeaderString(sent[0].Headers(), messaging.HeaderCorrelationID)
	assert.Equal(t, "abc", corr, "correlation-id must be preserved, not regenerated")
	tenant, _ := messaging.GetHeaderString(sent[0].Headers(), messaging.HeaderTenantID)
	assert.Equal(t, "t1", tenant, "tenant-id must survive the hop")
}

// TestTrustBridgeHeaders_Disabled_RegeneratesAndStrips pins the DEFAULT posture:
// every x-bridge.* header is stripped at ingress, the correlation ID is
// regenerated, and a spoofable tenant-id is dropped.
func TestTrustBridgeHeaders_Disabled_RegeneratesAndStrips(t *testing.T) {
	senderA := NewFakeSender()
	senderB := NewFakeSender()
	receiver, runner := newTrustHeadersRunner(false, senderA, senderB)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID:      "default-msg",
		Subject: "test",
		Headers: map[string]any{
			messaging.HeaderCorrelationID: "abc",
			messaging.HeaderTenantID:      "t1",
			messaging.HeaderRouteOverride: "bind-b",
		},
	})
	del := NewFakeDelivery(env)
	require.NoError(t, receiver.Emit(ctx, del))
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)

	require.Equal(t, 1, senderA.SentCount())
	require.Equal(t, 0, senderB.SentCount(), "inbound x-bridge.route-override must NOT steer routing")

	sent := senderA.GetSent()
	require.Len(t, sent, 1)
	corr, _ := messaging.GetHeaderString(sent[0].Headers(), messaging.HeaderCorrelationID)
	assert.NotEqual(t, "abc", corr, "correlation-id must be regenerated in the default posture")
	assert.NotEmpty(t, corr, "a fresh correlation-id must still be stamped")
	_, hasTenant := messaging.GetHeaderString(sent[0].Headers(), messaging.HeaderTenantID)
	assert.False(t, hasTenant, "tenant-id must be stripped in the default posture")
}
