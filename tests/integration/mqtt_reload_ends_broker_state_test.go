package integration_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// A live reload that changes or removes a persistent MQTT session's broker
// identity ends the broker session the old identity left behind (ADR 0024):
// a probe that connects as the old client ID afterwards finds no session. A
// shutdown ends nothing: the broker still holds the running identity's session.

// brokerStateSessionDef is one persistent MQTT session of a broker state
// reload configuration, with its receiver, sink sender, binding and route.
type brokerStateSessionDef struct{ id, clientID, topic string }

// brokerStateReloadConfig parses a configuration of the given sessions, each
// persistent with a 300 s session expiry, keeping managed subscription history
// in the SQLite store at historyPath.
func brokerStateReloadConfig(t *testing.T, version int, historyPath, brokerURL string, sessions ...brokerStateSessionDef) *ports.BridgeConfig {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "version: %d\nbridge:\n  id: broker-state-reload\n  deployment_mode: standalone\n", version)
	fmt.Fprintf(&b, "stores:\n  managed_subscriptions:\n    type: sqlite\n    options:\n      path: %q\n", historyPath)
	b.WriteString("sessions:\n")
	for _, s := range sessions {
		fmt.Fprintf(&b, "  - id: %s\n    transport: mqtt\n    session_mode: persistent\n    options:\n      session:\n"+
			"        broker_url: %q\n        client_id: %q\n        session_expiry_interval: 300\n"+
			"        connect_timeout: \"5s\"\n        keep_alive: 10\n", s.id, brokerURL, s.clientID)
	}
	b.WriteString("receivers:\n")
	for _, s := range sessions {
		fmt.Fprintf(&b, "  - id: rx-%s\n    transport: mqtt\n    session_id: %s\n    topics:\n      - {topic: %q, qos: 1}\n", s.id, s.id, s.topic)
	}
	b.WriteString("senders:\n")
	for _, s := range sessions {
		fmt.Fprintf(&b, "  - id: tx-%s\n    transport: sink\n", s.id)
	}
	b.WriteString("bindings:\n")
	for _, s := range sessions {
		fmt.Fprintf(&b, "  - {id: to-%s, sender_id: tx-%s, address: %s-sink}\n", s.id, s.id, s.id)
	}
	b.WriteString("routes:\n")
	for _, s := range sessions {
		fmt.Fprintf(&b, "  - id: route-%s\n    receiver_id: rx-%s\n    delivery_mode: direct_hold\n    bindings: [to-%s]\n"+
			"    policy:\n      allow_unfenced: true\n      on_permanent_failure: drop\n      on_expired: drop\n", s.id, s.id, s.id)
	}

	reg := ports.NewRegistry()
	if err := errors.Join(
		paho.Register(reg),
		nativestore.Register(reg),
		reg.Register("sink", func(ports.RawConfig) (ports.PluginConfig, error) { return rebuildSinkConfig{}, nil }),
	); err != nil {
		t.Fatalf("register decoders: %v", err)
	}
	cfg, err := cfgparser.Parse(strings.NewReader(b.String()), cfgparser.FormatYAML, reg)
	if err != nil {
		t.Fatalf("parse config: %v\n%s", err, b.String())
	}
	return cfg
}

// brokerStateHarness is a Supervisor over the real MQTT transport and a sink.
type brokerStateHarness struct {
	sup     *bridge.Supervisor
	sink    *rebuildSink
	changes chan *ports.BridgeConfig
	swaps   chan bridge.SwapEvent
	stop    func()
}

// startBrokerStateHarness seeds an empty managed subscription baseline for every
// session of cfg, as for new broker identities, and runs a Supervisor on cfg
// until every session has reconciled.
func startBrokerStateHarness(ctx context.Context, t *testing.T, cfg *ports.BridgeConfig) *brokerStateHarness {
	t.Helper()
	logs := &lockedLogBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("bridge log:\n%s", logs.String())
		}
	})
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	baselines := make(map[string][]string, len(cfg.Sessions))
	for _, s := range cfg.Sessions {
		baselines[s.ID] = nil
	}
	if err := bridge.NewBuilder(cfg).
		RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory()).
		SeedManagedSubscriptionBaselines(ctx, baselines); err != nil {
		t.Fatalf("seed managed subscription baselines: %v", err)
	}

	h := &brokerStateHarness{
		sink:    newRebuildSink(),
		changes: make(chan *ports.BridgeConfig, 1),
		swaps:   make(chan bridge.SwapEvent, 4),
	}
	h.sup = bridge.NewSupervisor(
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithSupervisorLogger(logger),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) { h.swaps <- ev }),
	)
	h.sup.RegisterTransport("mqtt", paho.NewFactory(logger))
	h.sup.RegisterTransport("sink", h.sink)
	h.sup.RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory())

	runCtx, stopRun := context.WithCancel(ctx)
	runErr := make(chan error, 1)
	go func() { runErr <- h.sup.Run(runCtx, cfg, h.changes) }()
	stopped := false
	h.stop = func() {
		if stopped {
			return
		}
		stopped = true
		stopRun()
		// Only that Run returns matters here, not its shutdown error.
		_ = wait.RequireReceive(t, runErr, 60*time.Second)
	}
	t.Cleanup(h.stop)
	waitForSupervisorRuntime(t, h.sup, runErr, 30*time.Second)
	h.waitReconciled(ctx, t, cfg)
	return h
}

// waitReconciled waits until every session of cfg has reconciled.
func (h *brokerStateHarness) waitReconciled(ctx context.Context, t *testing.T, cfg *ports.BridgeConfig) {
	t.Helper()
	for _, s := range cfg.Sessions {
		wait.Until(t, 30*time.Second, "session "+s.ID+" reconciled", func() bool {
			rt := h.sup.Runtime()
			return rt != nil && sessionReconciled(ctx, rt, s.ID)
		})
	}
}

// reload applies next and fails the test unless the swap succeeds.
func (h *brokerStateHarness) reload(ctx context.Context, t *testing.T, next *ports.BridgeConfig) {
	t.Helper()
	h.changes <- next
	ev := wait.RequireReceive(t, h.swaps, 60*time.Second)
	if ev.Error != nil {
		t.Fatalf("reload to version %d: %v", next.Version, ev.Error)
	}
	h.waitReconciled(ctx, t, next)
}

// delivered reports whether the sink sender senderID sent payload.
func (h *brokerStateHarness) delivered(senderID, payload string) bool {
	return h.sink.find(func(r rebuildSinkRecord) bool { return r.senderID == senderID && r.payload == payload })
}

// brokerSessionPresent connects as clientID without Clean Start and reports
// whether the broker still held a session for it. The probe's own session
// expiry is 0, so the probe leaves no session behind.
func brokerSessionPresent(ctx context.Context, t *testing.T, brokerURL, clientID string) bool {
	t.Helper()
	endpoint, err := url.Parse(brokerURL)
	if err != nil {
		t.Fatalf("parse MQTT broker URL: %v", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	present := make(chan bool, 1)
	cm, err := autopaho.NewConnection(probeCtx, autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{endpoint},
		KeepAlive:                     10,
		CleanStartOnInitialConnection: false,
		SessionExpiryInterval:         0,
		OnConnectionUp: func(_ *autopaho.ConnectionManager, connack *pahov5.Connack) {
			select {
			case present <- connack.SessionPresent:
			default:
			}
		},
		ClientConfig: pahov5.ClientConfig{ClientID: clientID},
	})
	if err != nil {
		t.Fatalf("new MQTT probe %q: %v", clientID, err)
	}
	defer func() { _ = cm.Disconnect(context.Background()) }()
	if err := cm.AwaitConnection(probeCtx); err != nil {
		t.Fatalf("connect MQTT probe %q: %v", clientID, err)
	}
	return wait.RequireReceive(t, present, 10*time.Second)
}

func TestSupervisor_ReloadThatChangesTheClientIDEndsTheOldBrokerSession(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	historyPath := filepath.Join(t.TempDir(), "history", "managed-subscriptions.db")
	topic := mqttlocal.UniqueClientID("broker-state-rename") + "/orders"
	oldClientID := mqttlocal.UniqueClientID("broker-state-old")
	newClientID := mqttlocal.UniqueClientID("broker-state-new")

	h := startBrokerStateHarness(ctx, t, brokerStateReloadConfig(t, 1, historyPath, brokerURL,
		brokerStateSessionDef{id: "orders", clientID: oldClientID, topic: topic}))

	h.reload(ctx, t, brokerStateReloadConfig(t, 2, historyPath, brokerURL,
		brokerStateSessionDef{id: "orders", clientID: newClientID, topic: topic}))

	if brokerSessionPresent(ctx, t, brokerURL, oldClientID) {
		t.Fatal("the broker still holds the session of the client ID the reload replaced")
	}
	publishRawMQTT(t, brokerURL, mqttlocal.UniqueClientID("broker-state-publisher"),
		&pahov5.Publish{Topic: topic, QoS: 1, Payload: []byte("after-rename")})
	wait.Until(t, 30*time.Second, "the renamed session carries traffic", func() bool {
		return h.delivered("tx-orders", "after-rename")
	})

	h.stop()
	if !brokerSessionPresent(ctx, t, brokerURL, newClientID) {
		t.Fatal("a shutdown must keep the running identity's broker session")
	}
}

func TestSupervisor_ReloadThatRemovesADurableSessionEndsItsBrokerSession(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	historyPath := filepath.Join(t.TempDir(), "history", "managed-subscriptions.db")
	root := mqttlocal.UniqueClientID("broker-state-remove")
	removed := brokerStateSessionDef{id: "orders", clientID: mqttlocal.UniqueClientID("broker-state-removed"), topic: root + "/orders"}
	kept := brokerStateSessionDef{id: "audit", clientID: mqttlocal.UniqueClientID("broker-state-kept"), topic: root + "/audit"}

	h := startBrokerStateHarness(ctx, t, brokerStateReloadConfig(t, 1, historyPath, brokerURL, removed, kept))

	h.reload(ctx, t, brokerStateReloadConfig(t, 2, historyPath, brokerURL, kept))

	if brokerSessionPresent(ctx, t, brokerURL, removed.clientID) {
		t.Fatal("the broker still holds the session of the durable session the reload removed")
	}
	publishRawMQTT(t, brokerURL, mqttlocal.UniqueClientID("broker-state-publisher"),
		&pahov5.Publish{Topic: kept.topic, QoS: 1, Payload: []byte("kept-after-remove")})
	wait.Until(t, 30*time.Second, "the kept session carries traffic", func() bool {
		return h.delivered("tx-audit", "kept-after-remove")
	})

	h.stop()
	if !brokerSessionPresent(ctx, t, brokerURL, kept.clientID) {
		t.Fatal("a shutdown must keep the running identity's broker session")
	}
}
