package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/runtime"
)

// ---------------------------------------------------------------------------
// RenderAddress
// ---------------------------------------------------------------------------

func TestRenderAddress_HappyPath(t *testing.T) {
	vars := map[string]any{"device_id": "42", "zone": "north"}

	got, err := runtime.RenderAddress("factory/a/orders/{device_id}", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "factory/a/orders/42" {
		t.Fatalf("got %q, want %q", got, "factory/a/orders/42")
	}
}

func TestRenderAddress_MultiplePlaceholders(t *testing.T) {
	vars := map[string]any{"zone": "north", "device": "sensor-3"}

	got, err := runtime.RenderAddress("{zone}/devices/{device}/data", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "north/devices/sensor-3/data" {
		t.Fatalf("got %q, want %q", got, "north/devices/sensor-3/data")
	}
}

func TestRenderAddress_NoPlaceholders(t *testing.T) {
	got, err := runtime.RenderAddress("static/topic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "static/topic" {
		t.Fatalf("got %q, want %q", got, "static/topic")
	}
}

func TestRenderAddress_EmptyTemplate(t *testing.T) {
	got, err := runtime.RenderAddress("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestRenderAddress_MissingPlaceholder(t *testing.T) {
	vars := map[string]any{"zone": "north"}

	_, err := runtime.RenderAddress("factory/{missing_key}/data", vars)
	if err == nil {
		t.Fatal("expected error for missing placeholder")
	}
}

func TestRenderAddress_EmptyPlaceholderKey(t *testing.T) {
	_, err := runtime.RenderAddress("factory/{}/data", nil)
	if err == nil {
		t.Fatal("expected error for empty placeholder key")
	}
}

func TestRenderAddress_RendersToEmpty(t *testing.T) {
	vars := map[string]any{"val": ""}

	_, err := runtime.RenderAddress("{val}", vars)
	if err == nil {
		t.Fatal("expected error when rendered result is empty")
	}
}

// ---------------------------------------------------------------------------
// ValidateMQTTTopic
// ---------------------------------------------------------------------------

func TestValidateMQTTTopic_ValidTopics(t *testing.T) {
	valid := []string{
		"devices/sensor-1/data",
		"factory/a/orders/42",
		"a",
		"a/b/c/d/e",
	}
	for _, topic := range valid {
		if err := runtime.ValidateMQTTTopic(topic); err != nil {
			t.Errorf("topic %q should be valid: %v", topic, err)
		}
	}
}

func TestValidateMQTTTopic_Empty(t *testing.T) {
	if err := runtime.ValidateMQTTTopic(""); err == nil {
		t.Fatal("empty topic should be rejected")
	}
}

func TestValidateMQTTTopic_PlusWildcard(t *testing.T) {
	if err := runtime.ValidateMQTTTopic("devices/+/data"); err == nil {
		t.Fatal("plus wildcard should be rejected")
	}
}

func TestValidateMQTTTopic_HashWildcard(t *testing.T) {
	if err := runtime.ValidateMQTTTopic("devices/#"); err == nil {
		t.Fatal("hash wildcard should be rejected")
	}
}

func TestValidateMQTTTopic_NullCharacter(t *testing.T) {
	if err := runtime.ValidateMQTTTopic("devices/\x00/data"); err == nil {
		t.Fatal("null character should be rejected")
	}
}

func TestValidateMQTTTopic_EmptySegment(t *testing.T) {
	if err := runtime.ValidateMQTTTopic("devices//data"); err == nil {
		t.Fatal("empty segment should be rejected")
	}
}

func TestValidateMQTTTopic_LeadingSlash(t *testing.T) {
	if err := runtime.ValidateMQTTTopic("/devices/data"); err == nil {
		t.Fatal("leading slash (empty first segment) should be rejected")
	}
}

func TestValidateMQTTTopic_TrailingSlash(t *testing.T) {
	if err := runtime.ValidateMQTTTopic("devices/data/"); err == nil {
		t.Fatal("trailing slash (empty last segment) should be rejected")
	}
}

// ---------------------------------------------------------------------------
// BindingResolver + MatchByHeader
// ---------------------------------------------------------------------------

func TestBindingResolver_MatchByHeader_SingleMatch(t *testing.T) {
	bindings := []domain.DestinationBinding{
		{ID: "bind-a", Transport: "mqtt", SessionID: "sess-a", Address: "factory/a/orders/{device_id}"},
		{ID: "bind-b", Transport: "mqtt", SessionID: "sess-b", Address: "factory/b/orders/{device_id}"},
	}
	headerMap := map[string]string{"A": "bind-a", "B": "bind-b"}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchByHeader("factory", headerMap))

	env := &domain.Envelope{
		ID:      "msg-1",
		Headers: map[string]any{"factory": "A", "device_id": "42"},
	}

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

func TestBindingResolver_MatchByHeader_NoMatch(t *testing.T) {
	bindings := []domain.DestinationBinding{
		{ID: "bind-a", Transport: "mqtt", Address: "topic/a"},
	}
	headerMap := map[string]string{"A": "bind-a"}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchByHeader("factory", headerMap))

	env := &domain.Envelope{
		ID:      "msg-2",
		Headers: map[string]any{"factory": "UNKNOWN"},
	}

	_, err := resolver.Resolve(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for no matching binding")
	}
	be, ok := domain.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected BridgeError, got %T", err)
	}
	if be.Class != domain.ErrorRejected {
		t.Fatalf("expected Rejected class, got %s", be.Class)
	}
}

func TestBindingResolver_MatchByHeader_MissingHeader(t *testing.T) {
	bindings := []domain.DestinationBinding{
		{ID: "bind-a", Transport: "mqtt", Address: "topic/a"},
	}
	headerMap := map[string]string{"A": "bind-a"}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchByHeader("factory", headerMap))

	env := &domain.Envelope{ID: "msg-3", Headers: map[string]any{}}

	_, err := resolver.Resolve(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when header is missing")
	}
}

// ---------------------------------------------------------------------------
// BindingResolver + MatchAll (fan-out)
// ---------------------------------------------------------------------------

func TestBindingResolver_MatchAll_FanOut(t *testing.T) {
	bindings := []domain.DestinationBinding{
		{ID: "bind-a", Transport: "mqtt", Address: "topic/a"},
		{ID: "bind-b", Transport: "mqtt", Address: "topic/b"},
		{ID: "bind-c", Transport: "sqs", Address: "https://sqs.example.com/queue"},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchAll())

	env := &domain.Envelope{ID: "msg-fanout"}

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

func TestBindingResolver_MatchByID(t *testing.T) {
	bindings := []domain.DestinationBinding{
		{ID: "bind-a", Address: "topic/a"},
		{ID: "bind-b", Address: "topic/b"},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchByID("bind-b"))

	env := &domain.Envelope{ID: "msg-id"}

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

func TestBindingResolver_MatchByID_NotFound(t *testing.T) {
	bindings := []domain.DestinationBinding{
		{ID: "bind-a", Address: "topic/a"},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchByID("nonexistent"))

	_, err := resolver.Resolve(context.Background(), &domain.Envelope{ID: "msg"})
	if err == nil {
		t.Fatal("expected error for non-existent binding ID")
	}
}

// ---------------------------------------------------------------------------
// BindingResolver -- MQTT topic validation
// ---------------------------------------------------------------------------

func TestBindingResolver_MQTTTopicValidation(t *testing.T) {
	bindings := []domain.DestinationBinding{
		{ID: "bind-bad", Transport: "mqtt", Address: "devices/{wildcard}/data"},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchAll())

	env := &domain.Envelope{
		ID:      "msg-bad",
		Headers: map[string]any{"wildcard": "sensor+"},
	}

	_, err := resolver.Resolve(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for MQTT topic with wildcard character")
	}
	if !errors.Is(err, domain.ErrInvalidTopic) {
		t.Fatalf("expected ErrInvalidTopic, got %v", err)
	}
}

func TestBindingResolver_NonMQTTSkipsTopicValidation(t *testing.T) {
	bindings := []domain.DestinationBinding{
		{ID: "bind-sqs", Transport: "sqs", Address: "queue+name"},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchAll())

	env := &domain.Envelope{ID: "msg-sqs"}

	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("SQS binding should not validate MQTT topics: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
}

// ---------------------------------------------------------------------------
// BindingResolver -- address template errors
// ---------------------------------------------------------------------------

func TestBindingResolver_AddressTemplateError(t *testing.T) {
	bindings := []domain.DestinationBinding{
		{ID: "bind-tmpl", Transport: "mqtt", Address: "factory/{missing}/data"},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchAll())

	env := &domain.Envelope{ID: "msg-tmpl", Headers: map[string]any{}}

	_, err := resolver.Resolve(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for missing template variable")
	}
}

// ---------------------------------------------------------------------------
// BindingResolver -- Options propagated as dispatch headers
// ---------------------------------------------------------------------------

func TestBindingResolver_OptionsAsDispatchHeaders(t *testing.T) {
	bindings := []domain.DestinationBinding{
		{
			ID:      "bind-opts",
			Address: "topic/a",
			Options: map[string]any{"qos": 1, "retain": true},
		},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchAll())

	env := &domain.Envelope{ID: "msg-opts"}

	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plans[0].Headers == nil {
		t.Fatal("expected dispatch headers from binding options")
	}
	if plans[0].Headers["qos"] != 1 {
		t.Fatalf("expected qos=1, got %v", plans[0].Headers["qos"])
	}
	if plans[0].Headers["retain"] != true {
		t.Fatalf("expected retain=true, got %v", plans[0].Headers["retain"])
	}
}

func TestBindingResolver_OptionsNotShared(t *testing.T) {
	opts := map[string]any{"qos": 1}
	bindings := []domain.DestinationBinding{
		{ID: "bind-shared", Address: "topic/a", Options: opts},
	}
	resolver := runtime.NewBindingResolver(bindings, runtime.MatchAll())

	plans, _ := resolver.Resolve(context.Background(), &domain.Envelope{ID: "msg"})
	plans[0].Headers["qos"] = 2

	if opts["qos"] != 1 {
		t.Fatal("modifying dispatch headers should not affect original binding options")
	}
}

// ---------------------------------------------------------------------------
// StaticResolver
// ---------------------------------------------------------------------------

func TestStaticResolver_ReturnsSamePlans(t *testing.T) {
	plans := []domain.DispatchPlan{
		{BindingID: "bind-1", Address: "topic/1"},
		{BindingID: "bind-2", Address: "topic/2"},
	}
	resolver := runtime.NewStaticResolver(plans...)

	got, err := resolver.Resolve(context.Background(), &domain.Envelope{ID: "any"})
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

func TestStaticResolver_IndependentOfEnvelope(t *testing.T) {
	resolver := runtime.NewStaticResolver(domain.DispatchPlan{BindingID: "b", Address: "t"})

	p1, _ := resolver.Resolve(context.Background(), &domain.Envelope{ID: "msg-1"})
	p2, _ := resolver.Resolve(context.Background(), &domain.Envelope{ID: "msg-2"})

	if p1[0].BindingID != p2[0].BindingID {
		t.Fatal("static resolver should return same plans regardless of envelope")
	}
}
