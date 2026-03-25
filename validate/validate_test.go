package validate_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/validate"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func validDirectHoldRoute() validate.RouteConfig {
	return validate.RouteConfig{
		ID: "dh-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			DispatchMode: domain.DispatchSingle,
		},
		Bindings: []domain.DestinationBinding{{
			ID:        "b1",
			SessionID: "sess-persistent",
			SenderID:  "sender-1",
			Transport: "mqtt",
		}},
		SourceCapabilities: []ports.Capability{
			ports.CapVisibilityExtension,
			ports.CapSourceRedelivery,
		},
		TargetTransport: "mqtt",
		TargetQoS:       1,
	}
}

func validSharedOutboxRoute() validate.RouteConfig {
	return validate.RouteConfig{
		ID: "so-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
			DispatchMode: domain.DispatchSingle,
		},
		Bindings: []domain.DestinationBinding{{
			ID:        "b1",
			SessionID: "sess-persistent",
			SenderID:  "sender-1",
			Transport: "mqtt",
		}},
		SourceGuaranteesID: true,
		TargetTransport:    "mqtt",
		TargetQoS:          1,
	}
}

func baseSessions() map[string]validate.SessionConfig {
	return map[string]validate.SessionConfig{
		"sess-persistent": {ID: "sess-persistent", Mode: domain.SessionPersistent},
		"sess-exclusive":  {ID: "sess-exclusive", Mode: domain.SessionExclusive},
		"sess-ephemeral":  {ID: "sess-ephemeral", Mode: domain.SessionEphemeral},
	}
}

func requireError(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected error containing %q, got %q", substr, err.Error())
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Valid configs pass
// ---------------------------------------------------------------------------

func TestValidate_ValidDirectHold(t *testing.T) {
	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{validDirectHoldRoute()},
		Sessions: baseSessions(),
	}
	requireNoError(t, validate.Validate(cfg))
}

func TestValidate_ValidSharedOutbox(t *testing.T) {
	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{validSharedOutboxRoute()},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		OutboxTransactionLimit: 100,
	}
	requireNoError(t, validate.Validate(cfg))
}

// ---------------------------------------------------------------------------
// DirectHold rejection scenarios
// ---------------------------------------------------------------------------

func TestValidate_DirectHold_NoVisibilityExtension(t *testing.T) {
	r := validDirectHoldRoute()
	r.SourceCapabilities = nil

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	requireError(t, err, "direct_hold invalid: source does not support visibility extension")
}

func TestValidate_DirectHold_FanOutEnabled(t *testing.T) {
	r := validDirectHoldRoute()
	r.Policy.DispatchMode = domain.DispatchFanOut

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	requireError(t, err, "direct_hold invalid: resolver fan-out is enabled")
}

func TestValidate_DirectHold_ExclusiveSession(t *testing.T) {
	r := validDirectHoldRoute()
	r.Bindings[0].SessionID = "sess-exclusive"

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	requireError(t, err, "direct_hold invalid: target session requires lease handoff")
}

// ---------------------------------------------------------------------------
// SharedOutbox rejection scenarios
// ---------------------------------------------------------------------------

func TestValidate_SharedOutbox_NoOutboxStore(t *testing.T) {
	r := validSharedOutboxRoute()

	cfg := validate.BridgeConfig{
		Routes:         []validate.RouteConfig{r},
		Sessions:       baseSessions(),
		HasOutboxStore: false,
		HasLeaseStore:  true,
	}
	err := validate.Validate(cfg)
	requireError(t, err, "shared_outbox invalid: no OutboxStore configured")
}

func TestValidate_SharedOutbox_NoLeaseStoreForExclusive(t *testing.T) {
	r := validSharedOutboxRoute()
	r.Bindings[0].SessionID = "sess-exclusive"

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          false,
		OutboxTransactionLimit: 100,
	}
	err := validate.Validate(cfg)
	requireError(t, err, "shared_outbox invalid: no LeaseStore configured for exclusive session")
}

func TestValidate_SharedOutbox_NoIdempotencyKey(t *testing.T) {
	r := validSharedOutboxRoute()
	r.SourceGuaranteesID = false
	r.HasIdempotencyProc = false

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		OutboxTransactionLimit: 100,
	}
	err := validate.Validate(cfg)
	requireError(t, err, "shared_outbox invalid: no idempotency key processor configured and source does not guarantee Envelope.ID")
}

func TestValidate_SharedOutbox_FanOutExceedsTransactionLimit(t *testing.T) {
	r := validSharedOutboxRoute()
	r.Policy.DispatchMode = domain.DispatchFanOut

	bindings := make([]domain.DestinationBinding, 101)
	for i := range bindings {
		bindings[i] = domain.DestinationBinding{
			ID:        fmt.Sprintf("b%d", i),
			SessionID: "sess-persistent",
			SenderID:  fmt.Sprintf("sender-%d", i),
			Transport: "mqtt",
		}
	}
	r.Bindings = bindings

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		OutboxTransactionLimit: 100,
	}
	err := validate.Validate(cfg)
	requireError(t, err, "shared_outbox invalid: fan-out cardinality exceeds OutboxStore transaction limit (100)")
}

func TestValidate_SharedOutbox_FanOutAtLimit_OK(t *testing.T) {
	r := validSharedOutboxRoute()
	r.Policy.DispatchMode = domain.DispatchFanOut

	bindings := make([]domain.DestinationBinding, 100)
	for i := range bindings {
		bindings[i] = domain.DestinationBinding{
			ID:        fmt.Sprintf("b%d", i),
			SessionID: "sess-persistent",
			SenderID:  fmt.Sprintf("sender-%d", i),
			Transport: "mqtt",
		}
	}
	r.Bindings = bindings

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		OutboxTransactionLimit: 100,
	}
	requireNoError(t, validate.Validate(cfg))
}

// ---------------------------------------------------------------------------
// MQTT QoS validation
// ---------------------------------------------------------------------------

func TestValidate_MQTT_QoS0_SharedOutbox(t *testing.T) {
	r := validSharedOutboxRoute()
	r.TargetQoS = 0

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		OutboxTransactionLimit: 100,
	}
	err := validate.Validate(cfg)
	requireError(t, err, "reliable MQTT route invalid: qos=0")
}

func TestValidate_MQTT_QoS0_DirectHold(t *testing.T) {
	r := validDirectHoldRoute()
	r.TargetQoS = 0
	r.Policy.RequireDurableEgress = true

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	requireError(t, err, "reliable MQTT route invalid: qos=0")
}

func TestValidate_MQTT_QoS2_OK(t *testing.T) {
	r := validSharedOutboxRoute()
	r.TargetQoS = 2

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		OutboxTransactionLimit: 100,
	}
	requireNoError(t, validate.Validate(cfg))
}

func TestValidate_NonMQTT_QoS0_OK(t *testing.T) {
	r := validSharedOutboxRoute()
	r.TargetTransport = "sqs"
	r.TargetQoS = 0

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		OutboxTransactionLimit: 100,
	}
	requireNoError(t, validate.Validate(cfg))
}

// ---------------------------------------------------------------------------
// Structural validation
// ---------------------------------------------------------------------------

func TestValidate_EmptyRouteID(t *testing.T) {
	r := validDirectHoldRoute()
	r.ID = ""

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	requireError(t, err, "route must have a non-empty ID")
}

func TestValidate_NoBindings(t *testing.T) {
	r := validDirectHoldRoute()
	r.Bindings = nil

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	requireError(t, err, "route must have at least one binding")
}

func TestValidate_UnknownSession(t *testing.T) {
	r := validDirectHoldRoute()
	r.Bindings[0].SessionID = "does-not-exist"

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	requireError(t, err, `binding "b1" references unknown session "does-not-exist"`)
}

func TestValidate_UnknownDeliveryMode(t *testing.T) {
	r := validDirectHoldRoute()
	r.Policy.DeliveryMode = "bogus"

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	requireError(t, err, `unrecognized delivery mode "bogus"`)
}

func TestValidate_UnknownDispatchMode(t *testing.T) {
	r := validDirectHoldRoute()
	r.Policy.DispatchMode = "bogus"

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	requireError(t, err, `unrecognized dispatch mode "bogus"`)
}

func TestValidate_EmptyDeliveryMode(t *testing.T) {
	r := validDirectHoldRoute()
	r.Policy.DeliveryMode = ""

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	requireError(t, err, "route must specify a delivery mode")
}

func TestValidate_BindingEmptySessionID_OK(t *testing.T) {
	r := validDirectHoldRoute()
	r.Bindings[0].SessionID = ""

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	requireNoError(t, validate.Validate(cfg))
}

// ---------------------------------------------------------------------------
// Multi-error aggregation
// ---------------------------------------------------------------------------

func TestValidate_MultipleErrors(t *testing.T) {
	r := validDirectHoldRoute()
	r.SourceCapabilities = nil
	r.Policy.DispatchMode = domain.DispatchFanOut

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	if err == nil {
		t.Fatal("expected errors, got nil")
	}

	var ve validate.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationErrors, got %T", err)
	}
	if len(ve) < 2 {
		t.Fatalf("expected at least 2 errors, got %d: %v", len(ve), ve)
	}
	requireError(t, err, "direct_hold invalid: source does not support visibility extension")
	requireError(t, err, "direct_hold invalid: resolver fan-out is enabled")
}

func TestValidate_MultipleRoutes_IndependentErrors(t *testing.T) {
	r1 := validDirectHoldRoute()
	r1.ID = "route-1"
	r1.SourceCapabilities = nil

	r2 := validSharedOutboxRoute()
	r2.ID = "route-2"

	cfg := validate.BridgeConfig{
		Routes:         []validate.RouteConfig{r1, r2},
		Sessions:       baseSessions(),
		HasOutboxStore: false,
		HasLeaseStore:  true,
	}
	err := validate.Validate(cfg)
	requireError(t, err, `route "route-1"`)
	requireError(t, err, `route "route-2"`)
}

// ---------------------------------------------------------------------------
// SharedOutbox with idempotency proc OK
// ---------------------------------------------------------------------------

func TestValidate_SharedOutbox_IdempotencyProc_OK(t *testing.T) {
	r := validSharedOutboxRoute()
	r.SourceGuaranteesID = false
	r.HasIdempotencyProc = true

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		OutboxTransactionLimit: 100,
	}
	requireNoError(t, validate.Validate(cfg))
}

// ---------------------------------------------------------------------------
// Empty config
// ---------------------------------------------------------------------------

func TestValidate_NoRoutes_OK(t *testing.T) {
	cfg := validate.BridgeConfig{
		Routes:   nil,
		Sessions: baseSessions(),
	}
	requireNoError(t, validate.Validate(cfg))
}

// ---------------------------------------------------------------------------
// Default transaction limit
// ---------------------------------------------------------------------------

func TestValidate_DirectHold_MQTT_QoS0_NotReliable_OK(t *testing.T) {
	r := validDirectHoldRoute()
	r.TargetQoS = 0
	r.Policy.RequireDurableEgress = false

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	requireNoError(t, validate.Validate(cfg))
}

func TestValidate_SharedOutbox_EmptyBindingSessionID(t *testing.T) {
	r := validSharedOutboxRoute()
	r.Bindings = []domain.DestinationBinding{{
		ID:        "b-no-session",
		SessionID: "",
		SenderID:  "sender-1",
		Transport: "mqtt",
	}}

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          false,
		OutboxTransactionLimit: 100,
	}
	requireNoError(t, validate.Validate(cfg))
}

func TestValidate_SharedOutbox_CustomTransactionLimit(t *testing.T) {
	r := validSharedOutboxRoute()
	r.Policy.DispatchMode = domain.DispatchFanOut

	bindings := make([]domain.DestinationBinding, 51)
	for i := range bindings {
		bindings[i] = domain.DestinationBinding{
			ID:        fmt.Sprintf("b%d", i),
			SessionID: "sess-persistent",
			SenderID:  fmt.Sprintf("sender-%d", i),
			Transport: "mqtt",
		}
	}
	r.Bindings = bindings

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		OutboxTransactionLimit: 50,
	}
	err := validate.Validate(cfg)
	requireError(t, err, "shared_outbox invalid: fan-out cardinality exceeds OutboxStore transaction limit (50)")
}

func TestValidate_MQTT_CaseInsensitiveTransport(t *testing.T) {
	r := validSharedOutboxRoute()
	r.TargetTransport = "MQTT"
	r.TargetQoS = 0

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		OutboxTransactionLimit: 100,
	}
	err := validate.Validate(cfg)
	requireError(t, err, "reliable MQTT route invalid: qos=0")
}

func TestValidate_EmptyID_CollectsMultipleStructuralErrors(t *testing.T) {
	r := validDirectHoldRoute()
	r.ID = ""
	r.Bindings = nil
	r.Policy.DeliveryMode = ""

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	if err == nil {
		t.Fatal("expected errors")
	}
	var ve validate.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationErrors, got %T", err)
	}
	if len(ve) < 3 {
		t.Fatalf("expected at least 3 structural errors, got %d: %v", len(ve), ve)
	}
}

func TestValidate_DefaultTransactionLimit(t *testing.T) {
	r := validSharedOutboxRoute()
	r.Policy.DispatchMode = domain.DispatchFanOut

	bindings := make([]domain.DestinationBinding, 101)
	for i := range bindings {
		bindings[i] = domain.DestinationBinding{
			ID:        fmt.Sprintf("b%d", i),
			SessionID: "sess-persistent",
			SenderID:  fmt.Sprintf("sender-%d", i),
			Transport: "mqtt",
		}
	}
	r.Bindings = bindings

	cfg := validate.BridgeConfig{
		Routes:         []validate.RouteConfig{r},
		Sessions:       baseSessions(),
		HasOutboxStore: true,
		HasLeaseStore:  true,
		// OutboxTransactionLimit left at zero => should default to 100
	}
	err := validate.Validate(cfg)
	requireError(t, err, "shared_outbox invalid: fan-out cardinality exceeds OutboxStore transaction limit (100)")
}
