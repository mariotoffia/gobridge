package integration_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ===============================================================
// Group 5: Real Transport Reconfiguration
//
// Validates that DynamoDB config changes produce actual transport
// reconfiguration using Docker-based SQS (ElasticMQ).
//
// Infrastructure:
//   ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
//   │  DynamoDB    │     │  ElasticMQ   │     │ config.Mgr  │
//   │  Local       │────▶│  (SQS)      │◀────│ + Supervisor│
//   └─────────────┘     └─────────────┘     └─────────────┘
// ===============================================================

// setTestAWSCredentials sets dummy AWS credentials for the SQS
// BridgeFactory which uses buildAWSConfig → LoadDefaultConfig.
// ElasticMQ does not validate credentials but the SDK requires them.
func setTestAWSCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
}

// sqsReceiverOpts creates config options for an SQS receiver.
func sqsReceiverOpts(queueURL, endpoint string) map[string]any {
	return map[string]any{
		"queue_url":          queueURL,
		"endpoint":           endpoint,
		"region":             "us-west-1",
		"max_messages":       int32(10),
		"wait_time_seconds":  int32(1),
		"visibility_timeout": int32(5),
	}
}

// sqsSenderOpts creates config options for an SQS sender.
func sqsSenderOpts(queueURL, endpoint string) map[string]any {
	return map[string]any{
		"queue_url": queueURL,
		"endpoint":  endpoint,
		"region":    "us-west-1",
	}
}

// TestDDBTransport_SQS_ConfigChangeSwapsQueue validates that a DDB
// overlay changing the SQS queue URL causes the supervisor to swap
// the runtime, with the new receiver consuming from the new queue.
//
// Scenario:
// -----------------------------------------------
//
//	Base: SQS rx-in (queue-A) → SQS tx-out (queue-B)
//	DDB v2: rx-in switches to queue-C
//	After swap: messages on queue-C reach queue-B
//
// -----------------------------------------------
func TestDDBTransport_SQS_ConfigChangeSwapsQueue(t *testing.T) {
	setTestAWSCredentials(t)
	sqsEP := sqslocal.Endpoint(t)
	sqsClient := sqslocal.Client(t)

	queueA := sqslocal.CreateQueue(t, sqsClient, sqslocal.UniqueQueue("cfgsqs-a"))
	queueB := sqslocal.CreateQueue(t, sqsClient, sqslocal.UniqueQueue("cfgsqs-b"))
	queueC := sqslocal.CreateQueue(t, sqsClient, sqslocal.UniqueQueue("cfgsqs-c"))

	loader := ddbConfigLoader(t, "tsqs-swap")
	ctx := context.Background()

	// Base config: route from queue-A to queue-B.
	baseCfg := &config.BridgeConfig{
		Bridge: config.BridgeSettings{
			ID:           "test-bridge",
			DrainTimeout: "1s",
		},
		Receivers: []config.ReceiverDef{
			{ID: "rx-in", Transport: "sqs", Options: sqsReceiverOpts(queueA, sqsEP)},
		},
		Senders: []config.SenderDef{
			{ID: "tx-out", Transport: "sqs", Options: sqsSenderOpts(queueB, sqsEP)},
		},
		Bindings: []config.BindingDef{
			{ID: "bind-1", SenderID: "tx-out", Address: queueB},
		},
		Routes: []config.RouteDef{
			{ID: "r1", ReceiverID: "rx-in", DeliveryMode: "direct_hold",
				Bindings: []string{"bind-1"},
				Policy:   config.PolicyDef{SendTimeout: "10s"}},
		},
	}

	// DDB overlay v1 is minimal.
	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	// Write base to a temp YAML file.
	basePath := writeBridgeConfigYAML(t, baseCfg)

	mgr := newTestConfigManager(t, basePath, loader)
	initialCfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	watchCh, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	eventCh := make(chan bridge.SwapEvent, 4)
	s := bridge.NewSupervisor(
		bridge.WithSupervisorLogger(slog.Default()),
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) { eventCh <- ev }),
	)
	s.RegisterTransport("sqs", sqsadapter.NewBridgeFactory(slog.Default()))
	s.RegisterTransport("fake", &cfgFakeTransportFactory{})
	s.RegisterStoreFactory("memory", &cfgFakeStoreFactory{})

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() {
		cancel()
		watchCancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	}()

	// Poll for runtime, but also check errCh for early failure.
	{
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if rt := s.Runtime(); rt != nil {
				break
			}
			select {
			case err := <-errCh:
				t.Fatalf("supervisor failed early: %v", err)
			default:
			}
			time.Sleep(10 * time.Millisecond) // SYNC: poll for supervisor runtime readiness
		}
		if s.Runtime() == nil {
			t.Fatal("timed out waiting for supervisor runtime")
		}
	}

	// Verify initial routing: send to queue-A, receive from queue-B.
	sendToSQS(t, sqsClient, queueA, `{"test":"initial"}`, nil)
	msgs := pollSQS(t, sqsClient, queueB, 1, 10*time.Second)
	if len(msgs) == 0 {
		t.Fatal("no message received on queue-B from initial config")
	}

	// DDB overlay v2: switch rx-in to queue-C.
	overlay2 := &config.BridgeConfig{
		Bridge: config.BridgeSettings{ID: "test-bridge"},
		Receivers: []config.ReceiverDef{
			{ID: "rx-in", Transport: "sqs", Options: sqsReceiverOpts(queueC, sqsEP)},
		},
	}
	if err := loader.Save(ctx, overlay2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	// Wait for swap to complete.
	select {
	case ev := <-eventCh:
		if ev.Error != nil {
			t.Fatalf("swap error: %v", ev.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for swap")
	}

	// Wait for the new runtime to be running before sending. The downstream
	// pollSQS has its own 10s timeout that absorbs any remaining poll-loop
	// startup delay, so we only need an observable "runtime running" gate.
	wait.Until(t, 2*time.Second, "post-swap runtime running", func() bool {
		rt := s.Runtime()
		return rt != nil && rt.IsRunning()
	})

	// Send to queue-C; should arrive at queue-B.
	sendToSQS(t, sqsClient, queueC, `{"test":"swapped"}`, nil)
	msgs = pollSQS(t, sqsClient, queueB, 1, 10*time.Second)
	if len(msgs) == 0 {
		t.Fatal("no message received on queue-B after swap to queue-C")
	}
}

// TestDDBTransport_SQS_NewRouteAdded validates that a DDB overlay
// adding a new SQS route results in both old and new routes working.
//
// Scenario:
// -----------------------------------------------
//
//	Base: r1 (queue-A → queue-B)
//	DDB v2: adds r2 (queue-C → queue-D)
//	After swap: both routes work
//
// -----------------------------------------------
func TestDDBTransport_SQS_NewRouteAdded(t *testing.T) {
	setTestAWSCredentials(t)
	sqsEP := sqslocal.Endpoint(t)
	sqsClient := sqslocal.Client(t)

	queueA := sqslocal.CreateQueue(t, sqsClient, sqslocal.UniqueQueue("cfgsqs2-a"))
	queueB := sqslocal.CreateQueue(t, sqsClient, sqslocal.UniqueQueue("cfgsqs2-b"))
	queueC := sqslocal.CreateQueue(t, sqsClient, sqslocal.UniqueQueue("cfgsqs2-c"))
	queueD := sqslocal.CreateQueue(t, sqsClient, sqslocal.UniqueQueue("cfgsqs2-d"))

	loader := ddbConfigLoader(t, "tsqs-add")
	ctx := context.Background()

	baseCfg := &config.BridgeConfig{
		Bridge: config.BridgeSettings{
			ID:           "test-bridge",
			DrainTimeout: "1s",
		},
		Receivers: []config.ReceiverDef{
			{ID: "rx-1", Transport: "sqs", Options: sqsReceiverOpts(queueA, sqsEP)},
		},
		Senders: []config.SenderDef{
			{ID: "tx-1", Transport: "sqs", Options: sqsSenderOpts(queueB, sqsEP)},
		},
		Bindings: []config.BindingDef{
			{ID: "bind-1", SenderID: "tx-1", Address: queueB},
		},
		Routes: []config.RouteDef{
			{ID: "r1", ReceiverID: "rx-1", DeliveryMode: "direct_hold",
				Bindings: []string{"bind-1"},
				Policy:   config.PolicyDef{SendTimeout: "10s"}},
		},
	}

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	basePath := writeBridgeConfigYAML(t, baseCfg)
	mgr := newTestConfigManager(t, basePath, loader)
	initialCfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	watchCh, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	eventCh := make(chan bridge.SwapEvent, 4)
	s := bridge.NewSupervisor(
		bridge.WithSupervisorLogger(slog.Default()),
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) { eventCh <- ev }),
	)
	s.RegisterTransport("sqs", sqsadapter.NewBridgeFactory(slog.Default()))
	s.RegisterStoreFactory("memory", &cfgFakeStoreFactory{})

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() {
		cancel()
		watchCancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	}()

	waitForSupervisorRuntime(t, s, 10*time.Second)

	// DDB overlay v2: add route r2 (queue-C → queue-D).
	overlay2 := &config.BridgeConfig{
		Bridge: config.BridgeSettings{ID: "test-bridge"},
		Receivers: []config.ReceiverDef{
			{ID: "rx-2", Transport: "sqs", Options: sqsReceiverOpts(queueC, sqsEP)},
		},
		Senders: []config.SenderDef{
			{ID: "tx-2", Transport: "sqs", Options: sqsSenderOpts(queueD, sqsEP)},
		},
		Bindings: []config.BindingDef{
			{ID: "bind-2", SenderID: "tx-2", Address: queueD},
		},
		Routes: []config.RouteDef{
			{ID: "r2", ReceiverID: "rx-2", DeliveryMode: "direct_hold",
				Bindings: []string{"bind-2"},
				Policy:   config.PolicyDef{SendTimeout: "10s"}},
		},
	}
	if err := loader.Save(ctx, overlay2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	select {
	case ev := <-eventCh:
		if ev.Error != nil {
			t.Fatalf("swap error: %v", ev.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for swap")
	}

	time.Sleep(500 * time.Millisecond) // STARTUP: let new routes initialize after config swap

	// Both routes should work.
	sendToSQS(t, sqsClient, queueA, `{"test":"r1"}`, nil)
	sendToSQS(t, sqsClient, queueC, `{"test":"r2"}`, nil)

	msgsB := pollSQS(t, sqsClient, queueB, 1, 10*time.Second)
	if len(msgsB) == 0 {
		t.Error("r1: no message on queue-B")
	}
	msgsD := pollSQS(t, sqsClient, queueD, 1, 10*time.Second)
	if len(msgsD) == 0 {
		t.Error("r2: no message on queue-D")
	}
}

// TestDDBTransport_ConfigRemovesRoute validates that when a DDB
// overlay drops a route, the old route's receiver/sender stop.
//
// Because mergeByID only adds/replaces (never removes from base),
// both routes must originate in the DDB overlay layer. The base
// YAML has no routes. When the overlay changes from r1+r2 to r1,
// the merged result has only r1.
//
// Scenario:
// -----------------------------------------------
//
//	Base: bridge settings only (no routes)
//	DDB v1: r1 (queue-A → queue-B) + r2 (queue-C → queue-D)
//	DDB v2: r1 only
//	After swap: r2 messages on queue-C are NOT forwarded
//
// -----------------------------------------------
func TestDDBTransport_ConfigRemovesRoute(t *testing.T) {
	setTestAWSCredentials(t)
	sqsEP := sqslocal.Endpoint(t)
	sqsClient := sqslocal.Client(t)

	queueA := sqslocal.CreateQueue(t, sqsClient, sqslocal.UniqueQueue("cfgsqs3-a"))
	queueB := sqslocal.CreateQueue(t, sqsClient, sqslocal.UniqueQueue("cfgsqs3-b"))
	queueC := sqslocal.CreateQueue(t, sqsClient, sqslocal.UniqueQueue("cfgsqs3-c"))
	queueD := sqslocal.CreateQueue(t, sqsClient, sqslocal.UniqueQueue("cfgsqs3-d"))

	loader := ddbConfigLoader(t, "tsqs-remove")
	ctx := context.Background()

	// Base config has only bridge settings — no routes.
	baseCfg := &config.BridgeConfig{
		Bridge: config.BridgeSettings{
			ID:           "test-bridge",
			DrainTimeout: "1s",
		},
	}

	// DDB overlay v1: both routes r1 and r2.
	overlayV1 := &config.BridgeConfig{
		Bridge: config.BridgeSettings{ID: "test-bridge"},
		Receivers: []config.ReceiverDef{
			{ID: "rx-1", Transport: "sqs", Options: sqsReceiverOpts(queueA, sqsEP)},
			{ID: "rx-2", Transport: "sqs", Options: sqsReceiverOpts(queueC, sqsEP)},
		},
		Senders: []config.SenderDef{
			{ID: "tx-1", Transport: "sqs", Options: sqsSenderOpts(queueB, sqsEP)},
			{ID: "tx-2", Transport: "sqs", Options: sqsSenderOpts(queueD, sqsEP)},
		},
		Bindings: []config.BindingDef{
			{ID: "bind-1", SenderID: "tx-1", Address: queueB},
			{ID: "bind-2", SenderID: "tx-2", Address: queueD},
		},
		Routes: []config.RouteDef{
			{ID: "r1", ReceiverID: "rx-1", DeliveryMode: "direct_hold",
				Bindings: []string{"bind-1"},
				Policy:   config.PolicyDef{SendTimeout: "10s"}},
			{ID: "r2", ReceiverID: "rx-2", DeliveryMode: "direct_hold",
				Bindings: []string{"bind-2"},
				Policy:   config.PolicyDef{SendTimeout: "10s"}},
		},
	}
	if err := loader.Save(ctx, overlayV1); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	basePath := writeBridgeConfigYAML(t, baseCfg)
	mgr := newTestConfigManager(t, basePath, loader)
	initialCfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	watchCh, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	eventCh := make(chan bridge.SwapEvent, 4)
	s := bridge.NewSupervisor(
		bridge.WithSupervisorLogger(slog.Default()),
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) { eventCh <- ev }),
	)
	s.RegisterTransport("sqs", sqsadapter.NewBridgeFactory(slog.Default()))
	s.RegisterStoreFactory("memory", &cfgFakeStoreFactory{})

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() {
		cancel()
		watchCancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	}()

	waitForSupervisorRuntime(t, s, 10*time.Second)

	// Verify r1 works initially.
	sendToSQS(t, sqsClient, queueA, `{"test":"r1-init"}`, nil)
	msgsB := pollSQS(t, sqsClient, queueB, 1, 10*time.Second)
	if len(msgsB) == 0 {
		t.Fatal("r1 initial: no message on queue-B")
	}

	// DDB overlay v2: only r1 (r2 removed from overlay entirely).
	overlayV2 := &config.BridgeConfig{
		Bridge: config.BridgeSettings{ID: "test-bridge"},
		Receivers: []config.ReceiverDef{
			{ID: "rx-1", Transport: "sqs", Options: sqsReceiverOpts(queueA, sqsEP)},
		},
		Senders: []config.SenderDef{
			{ID: "tx-1", Transport: "sqs", Options: sqsSenderOpts(queueB, sqsEP)},
		},
		Bindings: []config.BindingDef{
			{ID: "bind-1", SenderID: "tx-1", Address: queueB},
		},
		Routes: []config.RouteDef{
			{ID: "r1", ReceiverID: "rx-1", DeliveryMode: "direct_hold",
				Bindings: []string{"bind-1"},
				Policy:   config.PolicyDef{SendTimeout: "10s"}},
		},
	}
	if err := loader.Save(ctx, overlayV2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	select {
	case ev := <-eventCh:
		if ev.Error != nil {
			t.Fatalf("swap error: %v", ev.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for swap")
	}

	time.Sleep(500 * time.Millisecond) // STARTUP: let routes settle after config swap

	// r1 should still work.
	sendToSQS(t, sqsClient, queueA, `{"test":"r1-after"}`, nil)
	msgsB = pollSQS(t, sqsClient, queueB, 1, 10*time.Second)
	if len(msgsB) == 0 {
		t.Error("r1 after swap: no message on queue-B")
	}

	// r2 should NOT forward. Send to queue-C; queue-D should stay empty.
	sendToSQS(t, sqsClient, queueC, `{"test":"r2-removed"}`, nil)
	msgsD := pollSQS(t, sqsClient, queueD, 1, 3*time.Second)
	if len(msgsD) > 0 {
		t.Errorf("r2 after removal: unexpected message on queue-D: %v", msgsD)
	}
}
