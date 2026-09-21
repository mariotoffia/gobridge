package route

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// capturingDLQStore keeps every written entry so a test can assert the
// error code the runtime recorded.
type capturingDLQStore struct {
	mu      sync.Mutex
	entries []routing.DLQEntry
}

func (s *capturingDLQStore) Write(_ context.Context, e routing.DLQEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return nil
}
func (s *capturingDLQStore) Get(context.Context, string) (routing.DLQEntry, error) {
	return routing.DLQEntry{}, nil
}
func (s *capturingDLQStore) List(context.Context, routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}
func (s *capturingDLQStore) Delete(context.Context, []string) (int, error) { return 0, nil }
func (s *capturingDLQStore) DeleteByFilter(context.Context, routing.DLQFilter) (int, error) {
	return 0, nil
}
func (s *capturingDLQStore) Purge(context.Context, time.Time) (int, error) { return 0, nil }

func (s *capturingDLQStore) snapshot() []routing.DLQEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]routing.DLQEntry(nil), s.entries...)
}

// templateBinding needs a "tenant" header the test envelopes never carry.
var templateBinding = routing.DestinationBinding{ID: "b-tmpl", Address: "devices/{tenant}/events"}

func newTemplateRunner(policy routing.RoutePolicy, store ports.DLQStore, sender ports.Sender, rec *ports.RecordingExporter) *RouteRunner {
	return NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID:  "r-tmpl",
		Policy:   policy.WithDefaults(),
		Sender:   sender,
		Senders:  map[string]ports.Sender{templateBinding.ID: sender},
		DLQ:      dlq.New(store),
		Bindings: []routing.DestinationBinding{templateBinding},
		Metrics:  rec,
	})
}

func assertAddressTemplateDLQ(t *testing.T, store *capturingDLQStore, sender *countingSender, del *stubDelivery, rec *ports.RecordingExporter) {
	t.Helper()
	entries := store.snapshot()
	if len(entries) != 1 {
		t.Fatalf("DLQ entries = %d, want exactly 1", len(entries))
	}
	if got := entries[0].ErrorCode(); got != string(shared.ErrCodeAddressTemplate) {
		t.Fatalf("DLQ error code = %q, want %q", got, shared.ErrCodeAddressTemplate)
	}
	if del.retried {
		t.Fatal("a missing placeholder is rejected; the delivery must not be retried")
	}
	if !del.acked {
		t.Fatal("the delivery must be settled (acked) after the DLQ write")
	}
	if got := sender.calls.Load(); got != 0 {
		t.Fatalf("sender called %d times; an unrenderable address must never be sent", got)
	}
	if got := countTaggedCounter(rec, shared.MetricAddressTemplateErrors, shared.TagKeyRouteID, "r-tmpl"); got != 1 {
		t.Fatalf("AddressTemplateErrors{route_id=r-tmpl} = %d, want 1", got)
	}
}

// A missing placeholder on the resolver-less binding path is DLQ'd once with
// ADDRESS_TEMPLATE, never retried, and counted once.
func TestProcessDelivery_MissingPlaceholder_DLQdOnceAsAddressTemplate(t *testing.T) {
	rec := &ports.RecordingExporter{}
	store := &capturingDLQStore{}
	sender := &countingSender{}
	r := newTemplateRunner(routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}, store, sender, rec)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "tmpl-dlq", Subject: "s", Payload: []byte("p")})
	del := &stubDelivery{env: env}

	r.processDelivery(context.Background(), del)

	assertAddressTemplateDLQ(t, store, sender, del, rec)
}

// The direct_hold route-override path renders the overridden binding's
// address itself; a missing placeholder there is classified the same way.
func TestSendDirectHoldForBinding_MissingPlaceholder_DLQdOnceAsAddressTemplate(t *testing.T) {
	rec := &ports.RecordingExporter{}
	store := &capturingDLQStore{}
	sender := &countingSender{}
	r := newTemplateRunner(routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}, store, sender, rec)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "tmpl-override", Subject: "s", Payload: []byte("p")})
	del := &stubDelivery{env: env}

	if err := r.sendDirectHoldForBinding(context.Background(), del, env, templateBinding.ID); err != nil {
		t.Fatalf("sendDirectHoldForBinding returned error: %v", err)
	}

	assertAddressTemplateDLQ(t, store, sender, del, rec)
}

// Under on_permanent_failure=drop the message is dropped, not DLQ'd, and the
// address-template counter still increments once.
func TestProcessDelivery_MissingPlaceholder_DropPolicyCounted(t *testing.T) {
	rec := &ports.RecordingExporter{}
	store := &capturingDLQStore{}
	sender := &countingSender{}
	r := newTemplateRunner(routing.RoutePolicy{
		DeliveryMode:       routing.DeliveryDirectHold,
		OnPermanentFailure: routing.FailureDrop,
	}, store, sender, rec)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "tmpl-drop", Subject: "s", Payload: []byte("p")})
	del := &stubDelivery{env: env}

	r.processDelivery(context.Background(), del)

	if got := len(store.snapshot()); got != 0 {
		t.Fatalf("DLQ entries = %d, want 0 under drop policy", got)
	}
	if got := countDropReason(rec, "rejected"); got != 1 {
		t.Fatalf("MessagesDropped{reason=rejected} = %d, want 1", got)
	}
	if got := countTaggedCounter(rec, shared.MetricAddressTemplateErrors, shared.TagKeyRouteID, "r-tmpl"); got != 1 {
		t.Fatalf("AddressTemplateErrors{route_id=r-tmpl} = %d, want 1", got)
	}
	if del.retried || !del.acked {
		t.Fatalf("drop must settle terminally: acked=%v retried=%v", del.acked, del.retried)
	}
	if got := sender.calls.Load(); got != 0 {
		t.Fatalf("sender called %d times, want 0", got)
	}
}

// Any other rejected resolve error must not touch the address-template counter.
func TestHandleResolveError_OtherRejectedCode_NotCountedAsAddressTemplate(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := newTemplateRunner(routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}, &capturingDLQStore{}, &countingSender{}, rec)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "other-rejected", Payload: []byte("p")})

	if err := r.handleResolveError(context.Background(), &stubDelivery{env: env}, env, shared.ErrInvalidTopic.WithMessage("bad")); err != nil {
		t.Fatalf("handleResolveError returned error: %v", err)
	}
	if got := countCounter(rec, shared.MetricAddressTemplateErrors); got != 0 {
		t.Fatalf("AddressTemplateErrors = %d, want 0 for INVALID_TOPIC", got)
	}
}
