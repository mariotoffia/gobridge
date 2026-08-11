package runtime_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// ---------------------------------------------------------------------------
// RenderAddress
// ---------------------------------------------------------------------------

// Verifies RenderAddress substitutes a single placeholder from vars.
func TestRenderAddress_HappyPath(t *testing.T) {
	vars := map[string]any{"device_id": "42", "zone": "north"}

	got, err := route.RenderAddress("factory/a/orders/{device_id}", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "factory/a/orders/42" {
		t.Fatalf("got %q, want %q", got, "factory/a/orders/42")
	}
}

// Verifies RenderAddress substitutes multiple distinct placeholders in one template.
func TestRenderAddress_MultiplePlaceholders(t *testing.T) {
	vars := map[string]any{"zone": "north", "device": "sensor-3"}

	got, err := route.RenderAddress("{zone}/devices/{device}/data", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "north/devices/sensor-3/data" {
		t.Fatalf("got %q, want %q", got, "north/devices/sensor-3/data")
	}
}

// Verifies RenderAddress returns the template unchanged when it has no placeholders.
func TestRenderAddress_NoPlaceholders(t *testing.T) {
	got, err := route.RenderAddress("static/topic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "static/topic" {
		t.Fatalf("got %q, want %q", got, "static/topic")
	}
}

// Verifies RenderAddress accepts an empty template and returns an empty string.
func TestRenderAddress_EmptyTemplate(t *testing.T) {
	got, err := route.RenderAddress("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

// Verifies RenderAddress errors when a placeholder key is missing from vars.
func TestRenderAddress_MissingPlaceholder(t *testing.T) {
	vars := map[string]any{"zone": "north"}

	_, err := route.RenderAddress("factory/{missing_key}/data", vars)
	if err == nil {
		t.Fatal("expected error for missing placeholder")
	}
}

// Verifies RenderAddress rejects a template with an empty placeholder name.
func TestRenderAddress_EmptyPlaceholderKey(t *testing.T) {
	_, err := route.RenderAddress("factory/{}/data", nil)
	if err == nil {
		t.Fatal("expected error for empty placeholder key")
	}
}

// Verifies RenderAddress errors when substitution yields an empty result.
func TestRenderAddress_RendersToEmpty(t *testing.T) {
	vars := map[string]any{"val": ""}

	_, err := route.RenderAddress("{val}", vars)
	if err == nil {
		t.Fatal("expected error when rendered result is empty")
	}
}

// ---------------------------------------------------------------------------
// ValidateMQTTTopic — moved
// ---------------------------------------------------------------------------
//
// All ValidateMQTTTopic / TestValidateMQTTTopic_* tests have been moved to
// adapters/mqtt/transport/paho/topic_validator_test.go. The runtime no
// longer owns MQTT topic semantics —.

// ---------------------------------------------------------------------------
// BindingResolver + MatchByHeader
// ---------------------------------------------------------------------------

// Verifies MatchByHeader selects one binding from a header map and renders the address template.
func TestBindingResolver_MatchByHeader_SingleMatch(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "bind-a", Transport: "mqtt", SessionID: "sess-a", Address: "factory/a/orders/{device_id}"},
		{ID: "bind-b", Transport: "mqtt", SessionID: "sess-b", Address: "factory/b/orders/{device_id}"},
	}
	headerMap := map[string]string{"A": "bind-a", "B": "bind-b"}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchByHeader("factory", headerMap))

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-1",
		Headers: map[string]any{"factory": "A", "device_id": "42"},
	})

	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
	if plans[0].BindingID != "bind-a" {
		t.Fatalf("expected binding bind-a, got %s", plans[0].BindingID)
	}
	if plans[0].Address != "factory/a/orders/42" {
		t.Fatalf("expected rendered address, got %q", plans[0].Address)
	}
}

// Verifies MatchByHeader returns a rejected BridgeError when the header value maps to no binding.
func TestBindingResolver_MatchByHeader_NoMatch(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "bind-a", Transport: "mqtt", Address: "topic/a"},
	}
	headerMap := map[string]string{"A": "bind-a"}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchByHeader("factory", headerMap))

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-2",
		Headers: map[string]any{"factory": "UNKNOWN"},
	})

	_, err := resolver.Resolve(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for no matching binding")
	}
	be, ok := shared.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected BridgeError, got %T", err)
	}
	if be.Class != shared.ErrorRejected {
		t.Fatalf("expected Rejected class, got %s", be.Class)
	}
}

// Verifies MatchByHeader errors when the selector header is absent from the envelope.
func TestBindingResolver_MatchByHeader_MissingHeader(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "bind-a", Transport: "mqtt", Address: "topic/a"},
	}
	headerMap := map[string]string{"A": "bind-a"}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchByHeader("factory", headerMap))

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-3", Headers: map[string]any{}})

	_, err := resolver.Resolve(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when header is missing")
	}
}

// ---------------------------------------------------------------------------
// BindingResolver + MatchAll (fan-out)
// ---------------------------------------------------------------------------

// Verifies MatchAll returns one dispatch plan per binding including mixed transports.
func TestBindingResolver_MatchAll_FanOut(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "bind-a", Transport: "mqtt", Address: "topic/a"},
		{ID: "bind-b", Transport: "mqtt", Address: "topic/b"},
		{ID: "bind-c", Transport: "sqs", Address: "https://sqs.example.com/queue"},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchAll())

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-fanout"})

	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plans) != 3 {
		t.Fatalf("expected 3 plans for fan-out, got %d", len(plans))
	}

	ids := map[string]bool{}
	for _, p := range plans {
		ids[p.BindingID] = true
	}
	for _, id := range []string{"bind-a", "bind-b", "bind-c"} {
		if !ids[id] {
			t.Fatalf("missing binding %s in fan-out plans", id)
		}
	}
}

// ---------------------------------------------------------------------------
// BindingResolver + MatchByID
// ---------------------------------------------------------------------------

// Verifies MatchByID resolves only the binding with the configured ID.
func TestBindingResolver_MatchByID(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "bind-a", Address: "topic/a"},
		{ID: "bind-b", Address: "topic/b"},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchByID("bind-b"))

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-id"})

	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
	if plans[0].BindingID != "bind-b" {
		t.Fatalf("expected bind-b, got %s", plans[0].BindingID)
	}
	if plans[0].Address != "topic/b" {
		t.Fatalf("expected topic/b, got %s", plans[0].Address)
	}
}

// Verifies MatchByID errors when the binding ID is not in the list.
func TestBindingResolver_MatchByID_NotFound(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "bind-a", Address: "topic/a"},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchByID("nonexistent"))

	_, err := resolver.Resolve(context.Background(), messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg"}))
	if err == nil {
		t.Fatal("expected error for non-existent binding ID")
	}
}

// ---------------------------------------------------------------------------
// BindingResolver -- MQTT topic validation
// ---------------------------------------------------------------------------
//
// MQTT-specific resolver-level validation tests were removed as part of
// BindingResolver no longer performs transport-aware address
// validation. The route runner now invokes a per-binding
// ports.AddressValidator returned by TransportFactory.AddressValidator,
// so the equivalent end-to-end coverage lives next to the runner
// (runtime/route_address_validator_test.go) and inside the paho package.

// ---------------------------------------------------------------------------
// BindingResolver -- address template errors
// ---------------------------------------------------------------------------

// Verifies Resolve errors when a template variable is missing from the envelope headers.
func TestBindingResolver_AddressTemplateError(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "bind-tmpl", Transport: "mqtt", Address: "factory/{missing}/data"},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchAll())

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-tmpl", Headers: map[string]any{}})

	_, err := resolver.Resolve(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for missing template variable")
	}
}

// ---------------------------------------------------------------------------
// BindingResolver -- Options propagated as dispatch headers
// ---------------------------------------------------------------------------

// Verifies binding Headers are copied into dispatch plan headers with correct values.
func TestBindingResolver_HeadersAsDispatchHeaders(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{
			ID:      "bind-opts",
			Address: "topic/a",
			Headers: map[string]any{"qos": 1, "retain": true},
		},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchAll())

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-opts"})

	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plans[0].Headers == nil {
		t.Fatal("expected dispatch headers from binding headers")
	}
	if plans[0].Headers["qos"] != 1 {
		t.Fatalf("expected qos=1, got %v", plans[0].Headers["qos"])
	}
	if plans[0].Headers["retain"] != true {
		t.Fatalf("expected retain=true, got %v", plans[0].Headers["retain"])
	}
}

// Verifies mutating returned dispatch headers does not alter the original binding Headers map.
func TestBindingResolver_HeadersNotShared(t *testing.T) {
	opts := map[string]any{"qos": 1}
	bindings := []routing.DestinationBinding{
		{ID: "bind-shared", Address: "topic/a", Headers: opts},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchAll())

	plans, _ := resolver.Resolve(context.Background(), messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg"}))
	plans[0].Headers["qos"] = 2

	if opts["qos"] != 1 {
		t.Fatal("modifying dispatch headers should not affect original binding headers")
	}
}

// ---------------------------------------------------------------------------
// StaticResolver
// ---------------------------------------------------------------------------

// Verifies StaticResolver returns all configured plans unchanged.
func TestStaticResolver_ReturnsSamePlans(t *testing.T) {
	plans := []routing.DispatchPlan{
		{BindingID: "bind-1", Address: "topic/1"},
		{BindingID: "bind-2", Address: "topic/2"},
	}
	resolver := runtime.NewStaticResolver(plans...)

	got, err := resolver.Resolve(context.Background(), messaging.MustEnvelope(messaging.EnvelopeInput{ID: "any"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 plans, got %d", len(got))
	}
	if got[0].BindingID != "bind-1" || got[1].BindingID != "bind-2" {
		t.Fatalf("unexpected plan IDs: %s, %s", got[0].BindingID, got[1].BindingID)
	}
}

// Verifies StaticResolver yields identical plans for different envelope IDs.
func TestStaticResolver_IndependentOfEnvelope(t *testing.T) {
	resolver := runtime.NewStaticResolver(routing.DispatchPlan{BindingID: "b", Address: "t"})

	p1, _ := resolver.Resolve(context.Background(), messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"}))
	p2, _ := resolver.Resolve(context.Background(), messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-2"}))

	if p1[0].BindingID != p2[0].BindingID {
		t.Fatal("static resolver should return same plans regardless of envelope")
	}
}
