package bridge

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/require"
)

// closableReceiver is a fakeReceiver that also implements ports.ContextCloser
// (as the Service Bus / AMQP 1.0 / HTTP SSE receivers do) and records how many
// times it was closed.
type closableReceiver struct {
	fakeReceiver
	closes atomic.Int32
}

func (r *closableReceiver) Close(_ context.Context) error { r.closes.Add(1); return nil }

// closableSender is a fakeSender that also implements ports.ContextCloser and
// records how many times it was closed.
type closableSender struct {
	fakeSender
	closes atomic.Int32
}

func (s *closableSender) Close(_ context.Context) error { s.closes.Add(1); return nil }

// closableLinkFactory hands out a single context-closable receiver and sender so
// a test can assert they are torn down when complete() fails after building
// them.
type closableLinkFactory struct {
	fakeTransportFactory
	recv *closableReceiver
	send *closableSender
}

func (f *closableLinkFactory) NewReceiver(_ context.Context, _ ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	return f.recv, nil
}

func (f *closableLinkFactory) NewSender(_ context.Context, _ ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	return f.send, nil
}

// TestComplete_FailureClosesReceiversAndSenders covers: when complete()
// fails AFTER receivers and senders are built (here ValidateRoutes rejects a
// dlq-default route with no DLQ store), every receiver/sender that implements
// ports.ContextCloser must be closed. Otherwise the network clients / broker
// links they hold leak on every failed reload, because the runtime that would
// own and Stop them is never returned.
func TestComplete_FailureClosesReceiversAndSenders(t *testing.T) {
	ctx := context.Background()
	recv := &closableReceiver{}
	send := &closableSender{}

	cfg := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "b1"},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "closelink"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "closelink"}},
		Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx", Address: "queue://out"}},
		Routes: []ports.RouteDef{
			// dlq-default failure handling + no DLQ store => ValidateRoutes
			// rejects the route at the END of complete, after the receiver and
			// sender have already been constructed.
			{ID: "r1", ReceiverID: "rx", DeliveryMode: "direct_hold", Bindings: []string{"b1"}},
		},
	}

	b := NewBuilder(cfg).
		RegisterTransportFactory("closelink", &closableLinkFactory{recv: recv, send: send})

	prep, err := b.prepare(ctx)
	require.NoError(t, err)

	_, err = b.complete(ctx, prep)
	require.Error(t, err, "complete must reject a dlq-default route with no DLQ store")

	require.Equal(t, int32(1), recv.closes.Load(),
		"a receiver built before a complete() failure must be closed exactly once")
	require.Equal(t, int32(1), send.closes.Load(),
		"a sender built before a complete() failure must be closed exactly once")
}

// TestComplete_SuccessDoesNotCloseLinks guards defer against
// over-firing: a SUCCESSFUL complete must NOT close the receivers/senders — the
// returned runtime owns them and closes them on Stop.
func TestComplete_SuccessDoesNotCloseLinks(t *testing.T) {
	ctx := context.Background()
	recv := &closableReceiver{}
	send := &closableSender{}

	cfg := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "b1"},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "closelink"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "closelink"}},
		Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx", Address: "queue://out"}},
		Routes: []ports.RouteDef{
			{
				ID: "r1", ReceiverID: "rx", DeliveryMode: "direct_hold", Bindings: []string{"b1"},
				Policy: ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			},
		},
	}

	b := NewBuilder(cfg).
		RegisterTransportFactory("closelink", &closableLinkFactory{recv: recv, send: send})

	prep, err := b.prepare(ctx)
	require.NoError(t, err)

	rt, err := b.complete(ctx, prep)
	require.NoError(t, err)
	require.NotNil(t, rt)

	require.Equal(t, int32(0), recv.closes.Load(), "a successful complete must not close the receiver")
	require.Equal(t, int32(0), send.closes.Load(), "a successful complete must not close the sender")
}

// failSecondReceiverFactory returns a context-closable receiver on the FIRST
// NewReceiver call and an error on the second, so buildReceiversWithURIs fails
// mid-loop with receiver 0 already built (and holding a broker link).
type failSecondReceiverFactory struct {
	fakeTransportFactory
	first *closableReceiver
	calls atomic.Int32
}

func (f *failSecondReceiverFactory) NewReceiver(_ context.Context, _ ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	if f.calls.Add(1) == 1 {
		return f.first, nil
	}
	return nil, fmt.Errorf("boom: second receiver build fails")
}

// failSecondSenderFactory parallels failSecondReceiverFactory for senders.
type failSecondSenderFactory struct {
	fakeTransportFactory
	first *closableSender
	calls atomic.Int32
}

func (f *failSecondSenderFactory) NewSender(_ context.Context, _ ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	if f.calls.Add(1) == 1 {
		return f.first, nil
	}
	return nil, fmt.Errorf("boom: second sender build fails")
}

// TestComplete_PartialReceiverBuildClosesBuilt covers partial-build
// leak: buildReceiversWithURIs returns (nil, nil, err) when a LATER receiver
// fails, so complete's return-value-scoped receiver defer sees nil and cannot
// release the receivers already built this pass. The helper must close its own
// partial local map — receiver 0 (a ContextCloser holding a broker link) is
// closed exactly once.
func TestComplete_PartialReceiverBuildClosesBuilt(t *testing.T) {
	ctx := context.Background()
	first := &closableReceiver{}

	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "failrx"},
			{ID: "rx2", Transport: "failrx"},
		},
	}

	b := NewBuilder(cfg).
		RegisterTransportFactory("failrx", &failSecondReceiverFactory{first: first})

	prep, err := b.prepare(ctx)
	require.NoError(t, err)

	_, err = b.complete(ctx, prep)
	require.Error(t, err, "complete must fail when the second receiver build errors")

	require.Equal(t, int32(1), first.closes.Load(),
		"receiver built before a mid-loop build failure must be closed exactly once")
}

// TestComplete_PartialSenderBuildClosesBuilt is the sender mirror: with no
// receivers, buildReceiversWithURIs succeeds and buildSendersWithURIs fails on
// the second sender — the first sender must be closed exactly once.
func TestComplete_PartialSenderBuildClosesBuilt(t *testing.T) {
	ctx := context.Background()
	first := &closableSender{}

	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "failtx"},
			{ID: "tx2", Transport: "failtx"},
		},
	}

	b := NewBuilder(cfg).
		RegisterTransportFactory("failtx", &failSecondSenderFactory{first: first})

	prep, err := b.prepare(ctx)
	require.NoError(t, err)

	_, err = b.complete(ctx, prep)
	require.Error(t, err, "complete must fail when the second sender build errors")

	require.Equal(t, int32(1), first.closes.Load(),
		"sender built before a mid-loop build failure must be closed exactly once")
}
