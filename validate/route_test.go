package validate

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

func TestValidateStructural_EmptyID(t *testing.T) {
	cfg := BridgeConfig{}
	bc := cfg
	r := RouteConfig{
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Bindings: []routing.DestinationBinding{{ID: "b1"}},
	}
	bc.Routes = []RouteConfig{r}
	err := Validate(bc)
	if err == nil {
		t.Fatal("expected error for empty route ID")
	}
	if !strings.Contains(err.Error(), "non-empty ID") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateStructural_NoBindings(t *testing.T) {
	cfg := BridgeConfig{}
	r := RouteConfig{
		ID:     "r1",
		Policy: routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
	}
	cfg.Routes = []RouteConfig{r}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for no bindings")
	}
	if !strings.Contains(err.Error(), "at least one binding") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateStructural_UnknownDeliveryMode(t *testing.T) {
	cfg := BridgeConfig{}
	r := RouteConfig{
		ID:       "r1",
		Policy:   routing.RoutePolicy{DeliveryMode: "invalid_mode"},
		Bindings: []routing.DestinationBinding{{ID: "b1"}},
	}
	cfg.Routes = []RouteConfig{r}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for unknown delivery mode")
	}
	if !strings.Contains(err.Error(), "unrecognized delivery mode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateStructural_UnknownSession(t *testing.T) {
	cfg := BridgeConfig{
		Sessions: map[string]SessionConfig{},
	}
	r := RouteConfig{
		ID:     "r1",
		Policy: routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "missing-session"},
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	cfg.Routes = []RouteConfig{r}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
	if !strings.Contains(err.Error(), "unknown session") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateDirectHold_NoVisibilityExtension(t *testing.T) {
	cfg := BridgeConfig{}
	r := RouteConfig{
		ID:       "r1",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Bindings: []routing.DestinationBinding{{ID: "b1"}},
	}
	cfg.Routes = []RouteConfig{r}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for direct_hold without visibility extension")
	}
	if !strings.Contains(err.Error(), "visibility extension") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateDirectHold_FanOutRejected(t *testing.T) {
	cfg := BridgeConfig{}
	r := RouteConfig{
		ID: "r1",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			DispatchMode: routing.DispatchFanOut,
		},
		Bindings:           []routing.DestinationBinding{{ID: "b1"}},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	cfg.Routes = []RouteConfig{r}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for direct_hold with fan-out")
	}
	if !strings.Contains(err.Error(), "fan-out") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSharedOutbox_NoOutboxStore(t *testing.T) {
	cfg := BridgeConfig{}
	r := RouteConfig{
		ID:                 "r1",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Bindings:           []routing.DestinationBinding{{ID: "b1"}},
		HasIdempotencyProc: true,
	}
	cfg.Routes = []RouteConfig{r}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for shared_outbox without outbox store")
	}
	if !strings.Contains(err.Error(), "no OutboxStore") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSharedOutbox_NoIdempotencyProcOrSourceID(t *testing.T) {
	cfg := BridgeConfig{HasOutboxStore: true}
	r := RouteConfig{
		ID:       "r1",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Bindings: []routing.DestinationBinding{{ID: "b1"}},
	}
	cfg.Routes = []RouteConfig{r}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for shared_outbox without idempotency")
	}
	if !strings.Contains(err.Error(), "idempotency") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSharedOutbox_FanOutExceedsTransactionLimit(t *testing.T) {
	bindings := make([]routing.DestinationBinding, DefaultOutboxTransactionLimit+1)
	for i := range bindings {
		bindings[i] = routing.DestinationBinding{ID: "b" + string(rune('0'+i))}
	}

	cfg := BridgeConfig{HasOutboxStore: true}
	r := RouteConfig{
		ID: "r1",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			DispatchMode: routing.DispatchFanOut,
		},
		Bindings:           bindings,
		HasIdempotencyProc: true,
	}
	cfg.Routes = []RouteConfig{r}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for fan-out exceeding transaction limit")
	}
	if !strings.Contains(err.Error(), "transaction limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateMQTTQoS_ReliableWithQoS0(t *testing.T) {
	cfg := BridgeConfig{HasOutboxStore: true}
	r := RouteConfig{
		ID:                 "r1",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Bindings:           []routing.DestinationBinding{{ID: "b1"}},
		HasIdempotencyProc: true,
		TargetTransport:    "MQTT",
		TargetQoS:          0,
	}
	cfg.Routes = []RouteConfig{r}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for reliable MQTT route with qos=0")
	}
	if !strings.Contains(err.Error(), "qos=0") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_ValidDirectHold(t *testing.T) {
	cfg := BridgeConfig{}
	r := RouteConfig{
		ID:                 "r1",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Bindings:           []routing.DestinationBinding{{ID: "b1"}},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	cfg.Routes = []RouteConfig{r}
	err := Validate(cfg)
	if err != nil {
		t.Fatalf("expected no errors for valid direct_hold, got: %v", err)
	}
}

func TestValidate_ValidSharedOutbox(t *testing.T) {
	cfg := BridgeConfig{HasOutboxStore: true}
	r := RouteConfig{
		ID:                 "r1",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Bindings:           []routing.DestinationBinding{{ID: "b1"}},
		HasIdempotencyProc: true,
	}
	cfg.Routes = []RouteConfig{r}
	err := Validate(cfg)
	if err != nil {
		t.Fatalf("expected no errors for valid shared_outbox, got: %v", err)
	}
}
