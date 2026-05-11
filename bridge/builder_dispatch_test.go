package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatchTransportFactory is a fakeTransportFactory variant that returns
// a caller-supplied *fakeSender from NewSender so the test can read the
// captured OutboundMessages after Build/Start. It declares
// CapVisibilityExtension via the embedded fakeTransportFactory, which
// satisfies direct_hold capability validation.
type dispatchTransportFactory struct {
	fakeTransportFactory
	sender *fakeSender
}

func (f *dispatchTransportFactory) NewSender(_ context.Context, _ ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	return f.sender, nil
}

// TestT11_BridgeBuilder_PropagatesAddress_PreservesSubject is the T11
// acceptance test at the bridge.NewBuilder().Build() boundary. It asserts
// that:
//
//   - BindingDef.Address survives the builder path and lands on the
//     OutboundMessage.Address handed to the sender (transport destination).
//   - messaging.Envelope.Subject is preserved on the outbound envelope —
//     it is NOT replaced by the binding Address.
//   - The outbound envelope is a clone of the source delivery envelope
//     (separate pointer) so downstream mutation cannot leak back into
//     the source.
func TestT11_BridgeBuilder_PropagatesAddress_PreservesSubject(t *testing.T) {
	const (
		bindingAddress = "topic/test/T11"
		logicalSubject = "logical.subject.T11"
		envelopeID     = "msg-t11"
	)

	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "bridge-t11"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "sqs"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: bindingAddress},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx1",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"b1"},
			},
		},
	}

	sender := &fakeSender{done: make(chan struct{}, 1)}
	tf := &dispatchTransportFactory{sender: sender}

	rt, err := NewBuilder(cfg, WithBlueprintValidator(config.Validate)).
		RegisterTransportFactory("sqs", tf).
		Build(context.Background())
	require.NoError(t, err)
	require.NotNil(t, rt)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = rt.Stop(stopCtx)
	})

	src := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      envelopeID,
		Subject: logicalSubject,
		Payload: []byte("payload-t11"),
	})

	require.NoError(t, rt.Inject(ctx, "r1", src))

	select {
	case <-sender.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for sender to receive OutboundMessage")
	}

	captured := sender.snapshot()
	require.Len(t, captured, 1, "expected exactly one OutboundMessage")

	msg := captured[0]
	assert.Equal(t, bindingAddress, msg.Address,
		"BindingDef.Address must propagate end-to-end into OutboundMessage.Address")
	require.NotNil(t, msg.Envelope, "OutboundMessage.Envelope must not be nil")
	assert.Equal(t, logicalSubject, msg.Envelope.Subject(),
		"logical Envelope.Subject must be preserved (not replaced by Address)")
	assert.NotSame(t, src, msg.Envelope,
		"OutboundMessage.Envelope must be an isolated clone, not the source pointer")
	assert.Equal(t, logicalSubject, src.Subject(),
		"source envelope Subject must remain unmutated")
}

// TestT11_BridgeBuilder_RejectsEmptyBindingAddress verifies that the
// configured BlueprintValidator (config.Validate) is run before any
// transport, sender, or route is constructed and that an empty
// BindingDef.Address is rejected.
func TestT11_BridgeBuilder_RejectsEmptyBindingAddress(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "bridge-t11-neg"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "sqs"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: ""},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx1",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"b1"},
			},
		},
	}

	_, err := NewBuilder(cfg, WithBlueprintValidator(config.Validate)).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "address is required",
		"validator must reject an empty BindingDef.Address before send")
}
