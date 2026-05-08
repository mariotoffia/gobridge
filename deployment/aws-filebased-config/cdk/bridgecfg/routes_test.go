package bridgecfg_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
)

func TestWithRoute_AutoSyntheticBinding(t *testing.T) {
	qr := registry.NewQueueRegistry()
	cfg, err := bridgecfg.New("b").
		WithSQSReceiver("orders-in", qr.Ref("orders-in")).
		WithSQSSender("orders-out", qr.Ref("orders-out")).
		WithRoute("orders-in", "orders-out").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(cfg.Routes) != 1 {
		t.Fatalf("Routes length = %d, want 1", len(cfg.Routes))
	}
	if len(cfg.Bindings) != 1 {
		t.Fatalf("Bindings length = %d, want 1 (synthetic)", len(cfg.Bindings))
	}

	bind := cfg.Bindings[0]
	if bind.ID != "orders-out-binding" {
		t.Errorf("synthetic binding id = %q, want orders-out-binding", bind.ID)
	}
	if bind.SenderID != "orders-out" {
		t.Errorf("binding.SenderID = %q, want orders-out", bind.SenderID)
	}

	r := cfg.Routes[0]
	if r.ID != "orders-in-route" {
		t.Errorf("Route.ID = %q, want orders-in-route", r.ID)
	}
	if r.ReceiverID != "orders-in" {
		t.Errorf("Route.ReceiverID = %q, want orders-in", r.ReceiverID)
	}
	if len(r.Bindings) != 1 || r.Bindings[0] != "orders-out-binding" {
		t.Errorf("Route.Bindings = %v, want [orders-out-binding]", r.Bindings)
	}
}

func TestWithRoute_MultipleSendersFanOut(t *testing.T) {
	qr := registry.NewQueueRegistry()
	cfg, err := bridgecfg.New("b").
		WithSQSReceiver("in", qr.Ref("in")).
		WithSQSSender("a", qr.Ref("a")).
		WithSQSSender("b", qr.Ref("b")).
		WithRoute("in", "a", "b").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(cfg.Bindings) != 2 {
		t.Fatalf("Bindings length = %d, want 2", len(cfg.Bindings))
	}
	if len(cfg.Routes[0].Bindings) != 2 {
		t.Fatalf("Route.Bindings length = %d, want 2", len(cfg.Routes[0].Bindings))
	}
}

func TestWithRoute_RepeatedReceiverGetsUniqueID(t *testing.T) {
	qr := registry.NewQueueRegistry()
	cfg, err := bridgecfg.New("b").
		WithSQSReceiver("in", qr.Ref("in")).
		WithSQSSender("a", qr.Ref("a")).
		WithSQSSender("b", qr.Ref("b")).
		WithRoute("in", "a").
		// Second WithRoute on the same receiver — id collides on
		// "in-route", builder appends "-2".
		WithRouteOpts("in", []string{"b"}, nil).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("Routes length = %d, want 2", len(cfg.Routes))
	}
	if cfg.Routes[0].ID != "in-route" {
		t.Errorf("first route id = %q, want in-route", cfg.Routes[0].ID)
	}
	if cfg.Routes[1].ID != "in-route-2" {
		t.Errorf("second route id = %q, want in-route-2", cfg.Routes[1].ID)
	}
}

func TestWithRoute_EmptyReceiverErrors(t *testing.T) {
	_, err := bridgecfg.New("b").
		WithRoute("", "x").
		Build()
	if err == nil {
		t.Fatal("expected error on empty receiver id")
	}
	if !strings.Contains(err.Error(), "receiver id") {
		t.Errorf("error = %v, want one mentioning receiver id", err)
	}
}

func TestWithRoute_NoSendersErrors(t *testing.T) {
	_, err := bridgecfg.New("b").
		WithRoute("in").
		Build()
	if err == nil {
		t.Fatal("expected error on missing senders")
	}
	if !strings.Contains(err.Error(), "at least one sender") {
		t.Errorf("error = %v, want one mentioning missing sender", err)
	}
}

func TestWithRouteOpts_OptionsAndIDOverride(t *testing.T) {
	qr := registry.NewQueueRegistry()
	cfg, err := bridgecfg.New("b").
		WithSQSSender("out", qr.Ref("out")).
		WithRouteOpts("in", []string{"out"}, []bridgecfg.RouteOption{
			bridgecfg.WithRouteID("custom-route"),
			bridgecfg.WithRouteProcessors("p1", "p2"),
			bridgecfg.WithRouteDeliveryMode("direct_hold"),
		}).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	r := cfg.Routes[0]
	if r.ID != "custom-route" {
		t.Errorf("Route.ID = %q, want custom-route", r.ID)
	}
	if r.DeliveryMode != "direct_hold" {
		t.Errorf("DeliveryMode = %q, want direct_hold", r.DeliveryMode)
	}
	want := []string{"p1", "p2"}
	if len(r.Processors) != 2 || r.Processors[0] != want[0] || r.Processors[1] != want[1] {
		t.Errorf("Processors = %v, want %v", r.Processors, want)
	}
}
