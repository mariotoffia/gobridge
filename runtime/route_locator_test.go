package runtime_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/runtime"
)

func TestRouteLocator_NilWithoutLeaseStore(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("rl-no-lease"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	if rt.RouteLocator() != nil {
		t.Fatal("expected nil RouteLocator before Start without LeaseStore")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	if rt.RouteLocator() != nil {
		t.Fatal("expected nil RouteLocator without LeaseStore after Start")
	}
}

func TestRouteLocator_NonNilWithLeaseStoreAfterStart(t *testing.T) {
	lease := NewFakeLeaseStore()
	rt := runtime.New(
		runtime.WithInstanceID("rl-with-lease"),
		runtime.WithLeaseStore(lease),
	)
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	if rt.RouteLocator() != nil {
		t.Fatal("expected nil RouteLocator before Start (locator is created in Start)")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	if rt.RouteLocator() == nil {
		t.Fatal("expected non-nil RouteLocator with LeaseStore after Start")
	}
}
