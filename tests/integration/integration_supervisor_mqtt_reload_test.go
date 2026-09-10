package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/adapters/native/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ===========================================================================
// Config-driven reload through the GENERIC bridge.Supervisor with an MQTT
// destination that must reach REAL broker truth.
//
// Three-phase sequence, all driven by rewriting ONE config file that a
// poll-mode file watcher feeds into config.Manager → Supervisor.Run:
//
//	v1  healthy        broker_url = live Mosquitto      → route ready,
//	                   readiness ≥ LevelSubscribed, not degraded, and one
//	                   injected message reaches a real MQTT collector.
//	v2  broker-rejected broker_url = tcp://127.0.0.1:1  → the swap COMMITS
//	                   (apply-then-watch semantics), readiness stays below
//	                   LevelSubscribed, and after the convergence budget
//	                   (60s floor — the sessions are ephemeral, so
//	                   paho.Config.TransportFailoverTiming contributes 0 and
//	                   convergenceBudget(cfg) stays at the floor) the watch
//	                   marks the supervisor degraded with the "not
//	                   converged" reason carrying the config version.
//	v3  healthy again  broker_url = live Mosquitto      → new swap commits,
//	                   degraded clears, readiness returns to
//	                   ≥ LevelSubscribed, and recovery is proven by TRAFFIC:
//	                   an injected message reaches the collector again.
//
// The 60-second wait in phase v2 is real production behavior (the
// convergence budget floor), not test slack: time here IS the failure
// budget under test. All gates are deterministic (wait.Until / channels /
// WaitRouteReady / Degraded() / ReadinessLevel); no sleeps, no fake clocks.
//
// Config shape notes:
//   - The source is the inert "fake" receiver (blocks forever); traffic is
//     driven through the route pipeline via Runtime.Inject, which exercises
//     outbox persist → drain → the real MQTT sender against the real broker.
//   - The MQTT session is declared session_mode-less (ephemeral): a durable
//     (persistent/exclusive) session changing its broker URL is REFUSED at
//     reload preflight ("durable session broker identity changed"), while
//     the broker-convergence contract under test needs the v1→v2→v3 URL
//     swap to commit.
//   - The route carries an inline session block, which is what makes the
//     builder wire a runtime session manager for the MQTT session so its
//     broker connection drives Runtime.ReadinessLevel. An inline session is
//     always exclusive (lease handoff), which forces shared_outbox delivery
//     and real (in-memory) lease + outbox stores.
//   - connect_after_lease is explicitly false (the documented opt-out;
//     safe here — sender-only ephemeral session, no broker subscriptions to
//     resume): a deferred-connect session holding no lease is EXCLUDED from
//     readiness derivation (ports.ReadinessLevelFromDeepHealth), so with the
//     default the acquire→connect-fail→release cycle would make the v2
//     runtime read as a ready standby at LevelSubscribed and the
//     convergence watch would wrongly observe convergence. Eager connect
//     keeps the disconnected session counted, the honest broker-rejected
//     shape.
// ===========================================================================

const supervisorMQTTReloadConfigTemplate = `version: %d
bridge:
  id: sup-mqtt-reload
  deployment_mode: standalone
  shutdown_timeout: "5s"
  drain_timeout: "1s"
  log_level: "error"
stores:
  dlq:
    type: memory
  lease:
    type: memory
  outbox:
    type: memory
sessions:
  - id: mqtt-out
    transport: mqtt
    options:
      session:
        broker_url: "%s"
        client_id: "%s"
        connect_timeout: "3s"
        keep_alive: 5
receivers:
  - id: rx-src
    transport: fake
senders:
  - id: tx-mqtt
    transport: mqtt
    session_id: mqtt-out
bindings:
  - id: bind-mqtt
    sender_id: tx-mqtt
    address: "%s"
routes:
  - id: r1
    receiver_id: rx-src
    delivery_mode: shared_outbox
    bindings:
      - bind-mqtt
    session:
      session_id: mqtt-out
      sender_id: tx-mqtt
      drain_interval: "100ms"
      connect_after_lease: false
`

// writeSupervisorMQTTReloadConfig atomically replaces the watched config
// file (temp file + rename) so the poll watcher can never observe a
// half-written config.
func writeSupervisorMQTTReloadConfig(t *testing.T, path string, version int, brokerURL, clientID, topic string) {
	t.Helper()
	content := fmt.Sprintf(supervisorMQTTReloadConfigTemplate, version, brokerURL, clientID, topic)
	tmp, err := os.CreateTemp(filepath.Dir(path), "sup-mqtt-reload-*.yaml")
	if err != nil {
		t.Fatalf("create temp config: %v", err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		t.Fatalf("write temp config: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close temp config: %v", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatalf("rename config into place: %v", err)
	}
}

// newSupervisorMQTTReloadRegistry returns the integration-test registry
// plus the real paho decoder, so "transport: mqtt" parses into a typed
// *paho.Config exactly like the production composition roots do.
func newSupervisorMQTTReloadRegistry(t *testing.T) *ports.Registry {
	t.Helper()
	reg := newTestRegistry()
	if err := paho.Register(reg); err != nil {
		t.Fatalf("register paho decoder: %v", err)
	}
	return reg
}

// supMQTTReloadStoreFactory backs the "memory" store kind with REAL in-memory
// lease and outbox stores (not the no-op fakes): the shared_outbox route must
// actually persist and drain records for the traffic proof, and the exclusive
// session manager must actually hold a lease for the runtime to report an
// active role.
type supMQTTReloadStoreFactory struct{}

func (supMQTTReloadStoreFactory) NewLeaseStore(_ context.Context, _ ports.PluginConfig) (ports.LeaseStore, error) {
	return memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true)), nil
}

func (supMQTTReloadStoreFactory) NewOutboxStore(_ context.Context, _ ports.PluginConfig, _ ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return memoryoutbox.NewStore(), nil
}

func (supMQTTReloadStoreFactory) NewDLQStore(_ context.Context, _ ports.PluginConfig) (ports.DLQStore, error) {
	return &fakeDLQStore{}, nil
}

var _ ports.StoreFactory = (*supMQTTReloadStoreFactory)(nil)

func TestSupervisorMQTTReload_ConfigDrivenBrokerTruth(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t) // skip gate: -short / no Docker

	const unreachableBrokerURL = "tcp://127.0.0.1:1"
	topic := "itest/supreload/" + mqttlocal.UniqueClientID("t")
	clientID := mqttlocal.UniqueClientID("sup-reload")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")

	// The collector holds its own broker connection for the whole test; only
	// the BRIDGE is pointed at the dead URL in phase v2.
	collector := newMQTTCollector(t, topic, "sup-reload-col")

	collectorHasPayload := func(payload string) func() bool {
		return func() bool {
			for _, env := range collector.getMessages() {
				if string(env.Payload()) == payload {
					return true
				}
			}
			return false
		}
	}

	// ---- pipeline: file source + poll watcher → manager → supervisor ----

	writeSupervisorMQTTReloadConfig(t, cfgPath, 1, brokerURL, clientID, topic)

	reg := newSupervisorMQTTReloadRegistry(t)
	fileSource := fileconfig.NewSource(cfgPath, reg)
	watcher := fileconfig.NewWatcher(cfgPath, reg,
		fileconfig.WithMode(fileconfig.ModePoll),
		fileconfig.WithPollInterval(100*time.Millisecond),
	)
	mgr := config.NewManager(
		config.Layer{Name: "file", Loader: fileSource, Watcher: watcher},
	)
	t.Cleanup(mgr.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initialCfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("initial config load: %v", err)
	}

	swapEvents := make(chan bridge.SwapEvent, 8)
	sup := bridge.NewSupervisor(
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) {
			select {
			case swapEvents <- ev:
			default:
			}
		}),
	)
	sup.RegisterTransport("fake", &cfgFakeTransportFactory{})
	sup.RegisterTransport("mqtt", paho.NewFactory(nil))
	sup.RegisterStoreFactory("memory", supMQTTReloadStoreFactory{})

	watchCh, err := mgr.Watch(ctx)
	if err != nil {
		t.Fatalf("start config watch: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- sup.Run(ctx, initialCfg, watchCh) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(15 * time.Second):
			t.Error("supervisor did not exit after cancel")
		}
	}()

	waitSwap := func(desc string) bridge.SwapEvent {
		t.Helper()
		select {
		case ev := <-swapEvents:
			return ev
		case err := <-errCh:
			errCh <- err
			t.Fatalf("supervisor exited while waiting for swap (%s): %v", desc, err)
		case <-time.After(30 * time.Second):
			t.Fatalf("timed out waiting for swap: %s", desc)
		}
		return bridge.SwapEvent{}
	}

	// ================= Phase v1: healthy config, live broker =================

	rt1 := waitForSupervisorRuntime(t, sup, errCh, 30*time.Second)

	readyCtx, readyCancel := context.WithTimeout(ctx, 20*time.Second)
	if err := rt1.WaitRouteReady(readyCtx, "r1"); err != nil {
		readyCancel()
		t.Fatalf("v1: route r1 not ready: %v", err)
	}
	readyCancel()

	wait.Until(t, 30*time.Second, "v1 runtime reaches LevelSubscribed against live broker", func() bool {
		return rt1.ReadinessLevel(ctx) >= ports.LevelSubscribed
	})

	if degraded, reason := sup.Degraded(); degraded {
		t.Fatalf("v1: supervisor unexpectedly degraded: %s", reason)
	}
	if cfg := sup.Config(); cfg == nil || cfg.Version != 1 {
		t.Fatalf("v1: active config version = %+v, want 1", cfg)
	}

	// Prove broker truth by traffic: inject through the route pipeline and
	// receive on the real MQTT collector.
	v1Payload := `{"phase":"v1"}`
	if err := rt1.Inject(ctx, "r1", messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      uniqueID("sup-reload-v1"),
		Subject: topic,
		Payload: []byte(v1Payload),
	})); err != nil {
		t.Fatalf("v1: inject: %v", err)
	}
	wait.Until(t, 20*time.Second, "v1 message delivered to MQTT collector", collectorHasPayload(v1Payload))

	// ============ Phase v2: valid config, broker unreachable ============
	//
	// The reload is config-valid and COMMITS (Start returns before the MQTT
	// session ever reaches the broker); only the convergence watch may
	// surface the divergence, after the 60s budget floor.

	writeSupervisorMQTTReloadConfig(t, cfgPath, 2, unreachableBrokerURL, clientID, topic)

	ev2 := waitSwap("v2 broker-unreachable config")
	if ev2.Error != nil {
		t.Fatalf("v2: swap must COMMIT for a config-valid broker-unreachable reload, got error: %v", ev2.Error)
	}
	if ev2.Deferred {
		t.Fatal("v2: swap unexpectedly deferred")
	}
	if cfg := sup.Config(); cfg == nil || cfg.Version != 2 {
		t.Fatalf("v2: active config version = %+v, want 2", cfg)
	}
	rt2 := sup.Runtime()
	if rt2 == nil {
		t.Fatal("v2: no runtime after committed swap")
	}
	if rt2 == rt1 {
		t.Fatal("v2: runtime was not swapped")
	}
	// Immediately after the commit the supervisor is NOT degraded — the
	// convergence budget (60s) has not elapsed. This is the false
	// success shape the watch exists to expose.
	if degraded, reason := sup.Degraded(); degraded {
		t.Fatalf("v2: degraded immediately after commit (watch budget not yet elapsed): %s", reason)
	}
	if lvl := rt2.ReadinessLevel(ctx); lvl >= ports.LevelSubscribed {
		t.Fatalf("v2: readiness %s against unreachable broker, want below %s", lvl, ports.LevelSubscribed)
	}

	// Wait out the REAL convergence budget (60s floor + 2s poll cadence +
	// margin). Production behavior: the watch marks applied-but-not-converged.
	wait.Until(t, 100*time.Second, "v2 applied-but-not-converged marked degraded", func() bool {
		degraded, _ := sup.Degraded()
		return degraded
	})

	degraded, reason := sup.Degraded()
	if !degraded {
		t.Fatal("v2: degraded flag lost after being observed")
	}
	if !strings.Contains(reason, "not converged") {
		t.Fatalf("v2: degraded reason %q does not contain %q", reason, "not converged")
	}
	if !strings.Contains(reason, "config version 2") {
		t.Fatalf("v2: degraded reason %q does not carry the config version", reason)
	}
	if lvl := rt2.ReadinessLevel(ctx); lvl >= ports.LevelSubscribed {
		t.Fatalf("v2: readiness %s at degraded mark, want still below %s", lvl, ports.LevelSubscribed)
	}

	// ============== Phase v3: healthy config again, live broker ==============

	writeSupervisorMQTTReloadConfig(t, cfgPath, 3, brokerURL, clientID, topic)

	ev3 := waitSwap("v3 recovery config")
	if ev3.Error != nil {
		t.Fatalf("v3: swap error: %v", ev3.Error)
	}
	if cfg := sup.Config(); cfg == nil || cfg.Version != 3 {
		t.Fatalf("v3: active config version = %+v, want 3", cfg)
	}
	rt3 := sup.Runtime()
	if rt3 == nil {
		t.Fatal("v3: no runtime after committed swap")
	}

	// The successful swap clears the convergence-owned degraded state and the
	// new watch confirms convergence instead of superseding it with a mark.
	wait.Until(t, 30*time.Second, "v3 degraded cleared after recovery swap", func() bool {
		degraded, _ := sup.Degraded()
		return !degraded
	})

	readyCtx3, readyCancel3 := context.WithTimeout(ctx, 20*time.Second)
	if err := rt3.WaitRouteReady(readyCtx3, "r1"); err != nil {
		readyCancel3()
		t.Fatalf("v3: route r1 not ready: %v", err)
	}
	readyCancel3()

	wait.Until(t, 30*time.Second, "v3 runtime reconverges to LevelSubscribed", func() bool {
		return rt3.ReadinessLevel(ctx) >= ports.LevelSubscribed
	})

	// Recovery proven by traffic, not just flags.
	v3Payload := `{"phase":"v3"}`
	if err := rt3.Inject(ctx, "r1", messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      uniqueID("sup-reload-v3"),
		Subject: topic,
		Payload: []byte(v3Payload),
	})); err != nil {
		t.Fatalf("v3: inject: %v", err)
	}
	wait.Until(t, 20*time.Second, "v3 message delivered to MQTT collector", collectorHasPayload(v3Payload))

	// The degraded state must not resurface once converged: the v2 watcher is
	// obsolete (superseded runtime) and the v3 watcher observed convergence.
	if degraded, reason := sup.Degraded(); degraded {
		t.Fatalf("v3: degraded after full recovery: %s", reason)
	}
}
