package validate_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/validate"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func validDirectHoldRoute() validate.RouteConfig {
	return validate.RouteConfig{
		ID: "dh-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			DispatchMode: routing.DispatchSingle,
		},
		Bindings: []routing.DestinationBinding{{
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
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			DispatchMode: routing.DispatchSingle,
		},
		Bindings: []routing.DestinationBinding{{
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
		"sess-persistent": {ID: "sess-persistent", Mode: connectivity.SessionPersistent},
		"sess-exclusive":  {ID: "sess-exclusive", Mode: connectivity.SessionExclusive},
		"sess-ephemeral":  {ID: "sess-ephemeral", Mode: connectivity.SessionEphemeral},
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

// Verifies Validate accepts a direct_hold route when source capabilities and dispatch mode satisfy constraints.
func TestValidate_ValidDirectHold(t *testing.T) {
	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{validDirectHoldRoute()},
		Sessions: baseSessions(),
	}
	requireNoError(t, validate.Validate(cfg))
}

// Verifies Validate accepts a shared_outbox route when outbox, lease stores, and idempotency requirements are met.
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

// Verifies Validate rejects direct_hold when the source lacks visibility extension support.
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

// Verifies Validate rejects direct_hold when fan-out dispatch is enabled.
func TestValidate_DirectHold_FanOutEnabled(t *testing.T) {
	r := validDirectHoldRoute()
	r.Policy.DispatchMode = routing.DispatchFanOut

	cfg := validate.BridgeConfig{
		Routes:   []validate.RouteConfig{r},
		Sessions: baseSessions(),
	}
	err := validate.Validate(cfg)
	requireError(t, err, "direct_hold invalid: resolver fan-out is enabled")
}

// Verifies Validate rejects direct_hold when the binding uses an exclusive session requiring lease handoff.
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

// Verifies Validate rejects shared_outbox when no outbox store is configured.
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

// Verifies Validate rejects shared_outbox with an exclusive binding when no lease store is configured.
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

// Verifies Validate rejects shared_outbox when neither source ID guarantee nor idempotency processor is present.
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

// Verifies Validate rejects shared_outbox fan-out when binding count exceeds the outbox transaction limit.
func TestValidate_SharedOutbox_FanOutExceedsTransactionLimit(t *testing.T) {
	r := validSharedOutboxRoute()
	r.Policy.DispatchMode = routing.DispatchFanOut

	bindings := make([]routing.DestinationBinding, 101)
	for i := range bindings {
		bindings[i] = routing.DestinationBinding{
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

// Verifies Validate accepts shared_outbox fan-out when binding count equals the outbox transaction limit.
func TestValidate_SharedOutbox_FanOutAtLimit_OK(t *testing.T) {
	r := validSharedOutboxRoute()
	r.Policy.DispatchMode = routing.DispatchFanOut

	bindings := make([]routing.DestinationBinding, 100)
	for i := range bindings {
		bindings[i] = routing.DestinationBinding{
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

// Verifies Validate rejects reliable shared_outbox MQTT routes with QoS 0.
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

// Verifies Validate rejects reliable direct_hold MQTT routes with QoS 0 when durable egress is required.
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

// Verifies Validate accepts shared_outbox MQTT routes with QoS 2.
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

// Verifies Validate allows QoS 0 for non-MQTT target transports on shared_outbox routes.
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

// Verifies Validate rejects routes with an empty ID.
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

// Verifies Validate rejects routes with no bindings.
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

// Verifies Validate rejects bindings that reference a session ID absent from the session map.
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

// Verifies Validate rejects unrecognized delivery mode strings.
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

// Verifies Validate rejects unrecognized dispatch mode strings.
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

// Verifies Validate rejects routes with an unset delivery mode.
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

// Verifies Validate allows an empty binding session_id for direct_hold when other constraints pass.
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

// Verifies Validate aggregates multiple direct_hold violations into a single ValidationErrors value.
func TestValidate_MultipleErrors(t *testing.T) {
	r := validDirectHoldRoute()
	r.SourceCapabilities = nil
	r.Policy.DispatchMode = routing.DispatchFanOut

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

// Verifies Validate reports distinct errors for each route when multiple routes fail independent checks.
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

// Verifies Validate accepts shared_outbox without source ID guarantee when an idempotency processor is configured.
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

// Verifies Validate succeeds when the route list is empty.
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

// Verifies Validate allows MQTT QoS 0 on direct_hold when durable egress is not required.
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

// Verifies Validate accepts shared_outbox with an empty binding session_id when no exclusive lease is needed.
func TestValidate_SharedOutbox_EmptyBindingSessionID(t *testing.T) {
	r := validSharedOutboxRoute()
	r.Bindings = []routing.DestinationBinding{{
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

// Verifies Validate enforces fan-out cardinality against a custom outbox transaction limit.
func TestValidate_SharedOutbox_CustomTransactionLimit(t *testing.T) {
	r := validSharedOutboxRoute()
	r.Policy.DispatchMode = routing.DispatchFanOut

	bindings := make([]routing.DestinationBinding, 51)
	for i := range bindings {
		bindings[i] = routing.DestinationBinding{
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

// Verifies Validate treats MQTT transport names case-insensitively for reliable-route QoS rules.
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

// Verifies Validate aggregates several structural violations when route ID, bindings, and delivery mode are invalid together.
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

// Verifies Validate applies the default outbox transaction limit of 100 when the limit field is zero.
func TestValidate_DefaultTransactionLimit(t *testing.T) {
	r := validSharedOutboxRoute()
	r.Policy.DispatchMode = routing.DispatchFanOut

	bindings := make([]routing.DestinationBinding, 101)
	for i := range bindings {
		bindings[i] = routing.DestinationBinding{
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

// ---------------------------------------------------------------------------
// Store backend validation (deployment mode)
// ---------------------------------------------------------------------------

// Verifies Validate rejects clustered deployment when the lease store is not marked distributed.
func TestValidate_Clustered_NonDistributedLeaseStore(t *testing.T) {
	cfg := validate.BridgeConfig{
		Routes:                []validate.RouteConfig{validDirectHoldRoute()},
		Sessions:              baseSessions(),
		DeploymentMode:        "clustered",
		HasLeaseStore:         true,
		LeaseStoreDistributed: false,
	}
	err := validate.Validate(cfg)
	requireError(t, err, "clustered deployment requires a distributed LeaseStore")
}

// Verifies Validate rejects clustered deployment when the outbox store is not marked distributed.
func TestValidate_Clustered_NonDistributedOutboxStore(t *testing.T) {
	r := validSharedOutboxRoute()

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		DeploymentMode:         "clustered",
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		LeaseStoreDistributed:  true,
		OutboxStoreDistributed: false,
		OutboxTransactionLimit: 100,
	}
	err := validate.Validate(cfg)
	requireError(t, err, "clustered deployment requires a distributed OutboxStore")
}

// Verifies Validate rejects clustered deployment when a configured DLQ store is not marked distributed.
func TestValidate_Clustered_NonDistributedDLQStore(t *testing.T) {
	cfg := validate.BridgeConfig{
		Routes:              []validate.RouteConfig{validDirectHoldRoute()},
		Sessions:            baseSessions(),
		DeploymentMode:      "clustered",
		HasDLQStore:         true,
		DLQStoreDistributed: false,
	}
	err := validate.Validate(cfg)
	requireError(t, err, "clustered deployment requires a distributed DLQStore")
}

// Verifies Validate accepts clustered deployment when lease, outbox, and DLQ stores are all distributed.
func TestValidate_Clustered_DistributedStores_OK(t *testing.T) {
	r := validSharedOutboxRoute()

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		DeploymentMode:         "clustered",
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		HasDLQStore:            true,
		LeaseStoreDistributed:  true,
		OutboxStoreDistributed: true,
		DLQStoreDistributed:    true,
		OutboxTransactionLimit: 100,
	}
	requireNoError(t, validate.Validate(cfg))
}

// Verifies Validate accepts standalone deployment with non-distributed stores.
func TestValidate_Standalone_NonDistributedStores_OK(t *testing.T) {
	r := validSharedOutboxRoute()

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		DeploymentMode:         "standalone",
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		LeaseStoreDistributed:  false,
		OutboxStoreDistributed: false,
		OutboxTransactionLimit: 100,
	}
	requireNoError(t, validate.Validate(cfg))
}

// Verifies Validate accepts an unset deployment mode with non-distributed stores.
func TestValidate_EmptyDeploymentMode_NonDistributedStores_OK(t *testing.T) {
	r := validSharedOutboxRoute()

	cfg := validate.BridgeConfig{
		Routes:                 []validate.RouteConfig{r},
		Sessions:               baseSessions(),
		HasOutboxStore:         true,
		HasLeaseStore:          true,
		LeaseStoreDistributed:  false,
		OutboxStoreDistributed: false,
		OutboxTransactionLimit: 100,
	}
	requireNoError(t, validate.Validate(cfg))
}

// Verifies Validate accepts clustered deployment when no stores requiring distribution flags are present.
func TestValidate_Clustered_NoStores_OK(t *testing.T) {
	cfg := validate.BridgeConfig{
		Routes:         []validate.RouteConfig{validDirectHoldRoute()},
		Sessions:       baseSessions(),
		DeploymentMode: "clustered",
	}
	requireNoError(t, validate.Validate(cfg))
}
