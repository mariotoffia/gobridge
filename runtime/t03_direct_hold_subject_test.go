package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// captureSender records the full OutboundMessage so tests can assert on
// both the envelope and the destination Address.
type captureSender struct {
	mu   sync.Mutex
	msgs []ports.OutboundMessage
	done chan struct{}
}

func newCaptureSender() *captureSender {
	return &captureSender{done: make(chan struct{}, 1)}
}

func (s *captureSender) Send(_ context.Context, msg ports.OutboundMessage) error {
	s.mu.Lock()
	s.msgs = append(s.msgs, msg)
	s.mu.Unlock()
	select {
	case s.done <- struct{}{}:
	default:
	}
	return nil
}

func (s *captureSender) last() (ports.OutboundMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) == 0 {
		return ports.OutboundMessage{}, false
	}
	return s.msgs[len(s.msgs)-1], true
}

// TestT03_DirectHold_DoesNotMutateSourceEnvelopeSubject is the T03 acceptance
// test: direct-hold dispatch must not overwrite the source delivery
// envelope's Subject with the destination Address. The Address travels via
// OutboundMessage.Address, while the envelope's logical Subject is preserved.
func TestT03_DirectHold_DoesNotMutateSourceEnvelopeSubject(t *testing.T) {
	const (
		logicalSubject = "logical.event.subject"
		destAddress    = "destination/topic"
		bindingID      = "b1"
	)

	sender := newCaptureSender()

	bindings := []routing.DestinationBinding{{ID: bindingID, Address: destAddress}}
	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{{BindingID: bindingID}})
	resolver, err := runtime.NewRuleResolver(bindings, rules, "")
	if err != nil {
		t.Fatalf("NewRuleResolver: %v", err)
	}

	receiver := NewFakeReceiver()
	cfg := route.RouteRunnerConfig{
		RouteID:    "t03-route",
		Policy:     routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver:   receiver,
		Sender:     sender,
		DLQ:        dlq.New(NewFakeDLQStore()),
		Resolver:   resolver,
		Bindings:   bindings,
		InstanceID: "bridge-1",
	}
	runner := route.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	src := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-t03", Subject: logicalSubject, Payload: []byte("p")})
	del := NewFakeDelivery(src)

	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	select {
	case <-sender.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for send")
	}

	msg, ok := sender.last()
	if !ok {
		t.Fatal("no OutboundMessage captured")
	}

	if msg.Address != destAddress {
		t.Errorf("OutboundMessage.Address = %q, want %q", msg.Address, destAddress)
	}
	if msg.Envelope == nil {
		t.Fatal("OutboundMessage.Envelope is nil")
	}
	if msg.Envelope.Subject() != logicalSubject {
		t.Errorf("OutboundMessage.Envelope.Subject = %q, want %q (logical subject must be preserved)",
			msg.Envelope.Subject(), logicalSubject)
	}

	// The source delivery envelope must remain unmutated.
	if got := del.Envelope().Subject(); got != logicalSubject {
		t.Errorf("source delivery Envelope().Subject = %q, want %q (source must not be mutated)",
			got, logicalSubject)
	}
	if src.Subject() != logicalSubject {
		t.Errorf("source envelope Subject = %q, want %q (source must not be mutated)",
			src.Subject(), logicalSubject)
	}

	// The outbound envelope must not alias the source envelope, otherwise a
	// downstream sender mutating Subject could leak back into the source.
	if msg.Envelope == src {
		t.Error("OutboundMessage.Envelope aliases the source envelope; expected an isolated clone")
	}
}
