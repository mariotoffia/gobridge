package integration_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// A persistent MQTT session whose managed-subscription cleanup cannot quiesce
// is rebuilt in place by the Supervisor, and nothing else is touched.
//
// The trigger is real broker and runtime behaviour, nothing injected:
//
//   - The victim session's broker session already holds the stale filter and
//     the desired topic, and one QoS 1 message waits on the desired topic.
//   - Its managed history names only the stale filter, so the queued message is
//     not behind the cleanup gate: it reaches the victim route as soon as the
//     session connects, and the victim sink holds it unsettled.
//   - The reconcile then UNSUBSCRIBEs the stale filter, the broker answers 0x00,
//     and the cleanup waits for the held delivery to settle. It does not within
//     reconcile_timeout, so the session fails closed with the permanent
//     transport marker and no process-restart marker.
//
// The runtime reports the failed session after its rebuild backoff and the
// Supervisor retires and rebuilds that one unit inside the running runtime.
// The held send times out (send_timeout) and its in-process retry settles it,
// which lets the retire finish well inside the drain budget. The rebuilt
// session finds nothing it has to recycle for — the broker no longer holds the
// stale filter — so it reconciles cleanly and carries new traffic, while the
// bystander session, its route and its sender are never rebuilt.

const (
	rebuildVictimSessionID    = "mqtt-victim"
	rebuildBystanderSessionID = "mqtt-bystander"
	rebuildVictimSenderID     = "tx-victim"
	rebuildBystanderSenderID  = "tx-bystander"
)

const sessionRebuildConfigTemplate = `version: 1
bridge:
  id: session-rebuild
  deployment_mode: standalone
stores:
  managed_subscriptions:
    type: sqlite
    options:
      path: "{{HISTORY_PATH}}"
sessions:
  - id: mqtt-victim
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_url: "{{BROKER_URL}}"
        client_id: "{{VICTIM_CLIENT_ID}}"
        session_expiry_interval: 300
        connect_timeout: "5s"
        keep_alive: 10
        reconcile_timeout: "1s"
        unmatched_grace: "3s"
  - id: mqtt-bystander
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_url: "{{BROKER_URL}}"
        client_id: "{{BYSTANDER_CLIENT_ID}}"
        session_expiry_interval: 300
        connect_timeout: "5s"
        keep_alive: 10
receivers:
  - id: rx-victim
    transport: mqtt
    session_id: mqtt-victim
    topics:
      - {topic: "{{VICTIM_TOPIC}}", qos: 1}
  - id: rx-bystander
    transport: mqtt
    session_id: mqtt-bystander
    topics:
      - {topic: "{{BYSTANDER_TOPIC}}", qos: 1}
senders:
  - id: tx-victim
    transport: sink
  - id: tx-bystander
    transport: sink
bindings:
  - {id: to-victim-sink, sender_id: tx-victim, address: victim-sink}
  - {id: to-bystander-sink, sender_id: tx-bystander, address: bystander-sink}
routes:
  - id: victim
    receiver_id: rx-victim
    delivery_mode: direct_hold
    bindings: [to-victim-sink]
    policy:
      allow_unfenced: true
      on_permanent_failure: drop
      on_expired: drop
      send_timeout: "5s"
      send_retry_budget: "10s"
  - id: bystander
    receiver_id: rx-bystander
    delivery_mode: direct_hold
    bindings: [to-bystander-sink]
    policy:
      allow_unfenced: true
      on_permanent_failure: drop
      on_expired: drop
`

func TestSupervisor_ManagedCleanupQuiescenceFailureRebuildsOnlyThatSession(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)

	logs := &lockedLogBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("bridge log:\n%s", logs.String())
		}
	})
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	root := mqttlocal.UniqueClientID("session-rebuild")
	staleFilter := root + "/stale"
	victimTopic := root + "/victim"
	bystanderTopic := root + "/bystander"
	victimClientID := mqttlocal.UniqueClientID("session-rebuild-victim")
	bystanderClientID := mqttlocal.UniqueClientID("session-rebuild-bystander")
	historyPath := filepath.Join(t.TempDir(), "history", "managed-subscriptions.db")

	reg := ports.NewRegistry()
	if err := errors.Join(
		paho.Register(reg),
		nativestore.Register(reg),
		reg.Register("sink", func(ports.RawConfig) (ports.PluginConfig, error) { return rebuildSinkConfig{}, nil }),
	); err != nil {
		t.Fatalf("register decoders: %v", err)
	}
	yaml := strings.NewReplacer(
		"{{HISTORY_PATH}}", historyPath,
		"{{BROKER_URL}}", brokerURL,
		"{{VICTIM_CLIENT_ID}}", victimClientID,
		"{{BYSTANDER_CLIENT_ID}}", bystanderClientID,
		"{{VICTIM_TOPIC}}", victimTopic,
		"{{BYSTANDER_TOPIC}}", bystanderTopic,
	).Replace(sessionRebuildConfigTemplate)
	cfg, err := cfgparser.Parse(strings.NewReader(yaml), cfgparser.FormatYAML, reg)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	// Broker state an earlier configuration left behind: the victim's broker
	// session still holds the stale filter, and a message waits for it on the
	// topic it still wants.
	seedPersistentBrokerSession(ctx, t, brokerURL, victimClientID, staleFilter, victimTopic)
	seedPersistentBrokerSession(ctx, t, brokerURL, bystanderClientID, bystanderTopic)
	const backlogPayload = "victim-backlog"
	publishRawMQTT(t, brokerURL, mqttlocal.UniqueClientID("session-rebuild-publisher"),
		&pahov5.Publish{Topic: victimTopic, QoS: 1, Payload: []byte(backlogPayload)})

	// The managed history the victim's previous configuration wrote: only the
	// filter the new configuration removes. Seeded through the bridge so the
	// storage identity is the one the running session derives.
	if err := bridge.NewBuilder(cfg).
		RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory()).
		SeedManagedSubscriptionBaselines(ctx, map[string][]string{
			rebuildVictimSessionID:    {staleFilter},
			rebuildBystanderSessionID: {bystanderTopic},
		}); err != nil {
		t.Fatalf("seed managed subscription history: %v", err)
	}

	metrics := &ports.RecordingExporter{}
	sink := newRebuildSink()
	mqtt := &sessionCountingMQTTFactory{Factory: paho.NewFactory(logger), built: make(map[string]int)}
	sup := bridge.NewSupervisor(
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithSupervisorMetrics(metrics),
		bridge.WithSupervisorLogger(logger),
	)
	sup.RegisterTransport("mqtt", mqtt)
	sup.RegisterTransport("sink", sink)
	sup.RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory())

	runCtx, stopRun := context.WithCancel(ctx)
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(runCtx, cfg, nil) }()
	t.Cleanup(func() {
		stopRun()
		// Only that Run returns matters here, not its shutdown error.
		_ = wait.RequireReceive(t, runErr, 60*time.Second)
	})
	rt := waitForSupervisorRuntime(t, sup, runErr, 30*time.Second)

	// The bystander carries traffic from the start.
	wait.Until(t, 30*time.Second, "bystander session reconciled", func() bool {
		return sessionReconciled(ctx, rt, rebuildBystanderSessionID)
	})
	publishRawMQTT(t, brokerURL, mqttlocal.UniqueClientID("session-rebuild-publisher"),
		&pahov5.Publish{Topic: bystanderTopic, QoS: 1, Payload: []byte("bystander-before")})
	wait.Until(t, 30*time.Second, "bystander sink receives the first message", func() bool {
		return sink.recorded(rebuildBystanderSenderID, 1, "bystander-before")
	})

	// The victim route holds the queued message, so the cleanup cannot quiesce.
	wait.RequireClosed(t, sink.held, 30*time.Second)
	if got := sink.heldPayload(); got != backlogPayload {
		t.Fatalf("victim sink held %q, want the queued message %q", got, backlogPayload)
	}

	wait.Until(t, 60*time.Second, "victim session reported for an in-place rebuild", func() bool {
		return sessionRebuilds(metrics, rebuildVictimSessionID) >= 1
	})

	// The rebuilt victim session reconciles and carries new traffic through a
	// sender built for the rebuilt unit.
	wait.Until(t, 60*time.Second, "rebuilt victim session reconciled", func() bool {
		return sink.instances(rebuildVictimSenderID) >= 2 && sessionReconciled(ctx, rt, rebuildVictimSessionID)
	})
	publishRawMQTT(t, brokerURL, mqttlocal.UniqueClientID("session-rebuild-publisher"),
		&pahov5.Publish{Topic: victimTopic, QoS: 1, Payload: []byte("victim-after")})
	wait.Until(t, 30*time.Second, "rebuilt victim sender receives the new message", func() bool {
		return sink.recordedFrom(rebuildVictimSenderID, 2, "victim-after")
	})

	// The bystander was never rebuilt and still carries traffic.
	publishRawMQTT(t, brokerURL, mqttlocal.UniqueClientID("session-rebuild-publisher"),
		&pahov5.Publish{Topic: bystanderTopic, QoS: 1, Payload: []byte("bystander-after")})
	wait.Until(t, 30*time.Second, "bystander sink receives the second message", func() bool {
		return sink.recorded(rebuildBystanderSenderID, 1, "bystander-after")
	})

	if got := sessionRebuilds(metrics, rebuildVictimSessionID); got != 1 {
		t.Fatalf("victim session rebuilds = %d, want 1", got)
	}
	if got := sessionRebuilds(metrics, rebuildBystanderSessionID); got != 0 {
		t.Fatalf("bystander session rebuilds = %d, want 0", got)
	}
	if victim, bystander := mqtt.sessionsBuilt(rebuildVictimSessionID), mqtt.sessionsBuilt(rebuildBystanderSessionID); victim != 2 || bystander != 1 {
		t.Fatalf("MQTT sessions built: victim %d, bystander %d; want 2 and 1", victim, bystander)
	}
	if victim, bystander := sink.instances(rebuildVictimSenderID), sink.instances(rebuildBystanderSenderID); victim != 2 || bystander != 1 {
		t.Fatalf("sink senders built: victim %d, bystander %d; want 2 and 1", victim, bystander)
	}
	if sup.Runtime() != rt {
		t.Fatal("the rebuild replaced the running runtime; it must rebuild in place")
	}
	if sup.Terminal() {
		t.Fatal("supervisor is terminal after an in-place session rebuild")
	}
	select {
	case err := <-runErr:
		runErr <- err
		t.Fatalf("supervisor Run returned during the rebuild: %v", err)
	default:
	}
}

// seedPersistentBrokerSession leaves a persistent broker session for clientID
// that holds filters at QoS 1, the way an earlier bridge configuration would.
func seedPersistentBrokerSession(ctx context.Context, t *testing.T, brokerURL, clientID string, filters ...string) {
	t.Helper()
	endpoint, err := url.Parse(brokerURL)
	if err != nil {
		t.Fatalf("parse MQTT broker URL: %v", err)
	}
	seedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cm, err := autopaho.NewConnection(seedCtx, autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{endpoint},
		KeepAlive:                     10,
		CleanStartOnInitialConnection: true,
		SessionExpiryInterval:         300,
		ClientConfig:                  pahov5.ClientConfig{ClientID: clientID},
	})
	if err != nil {
		t.Fatalf("new MQTT client %q: %v", clientID, err)
	}
	if err := cm.AwaitConnection(seedCtx); err != nil {
		_ = cm.Disconnect(context.Background())
		t.Fatalf("connect MQTT client %q: %v", clientID, err)
	}
	subscriptions := make([]pahov5.SubscribeOptions, 0, len(filters))
	for _, filter := range filters {
		subscriptions = append(subscriptions, pahov5.SubscribeOptions{Topic: filter, QoS: 1})
	}
	suback, err := cm.Subscribe(seedCtx, &pahov5.Subscribe{Subscriptions: subscriptions})
	if err != nil {
		_ = cm.Disconnect(context.Background())
		t.Fatalf("subscribe MQTT client %q to %v: %v", clientID, filters, err)
	}
	for i, reason := range suback.Reasons {
		if reason != 1 {
			_ = cm.Disconnect(context.Background())
			t.Fatalf("SUBACK for %q = 0x%02x, want QoS 1 granted", filters[i], reason)
		}
	}
	// A normal DISCONNECT keeps the session for its expiry interval.
	if err := cm.Disconnect(seedCtx); err != nil {
		t.Fatalf("disconnect MQTT client %q: %v", clientID, err)
	}
}

// sessionReconciled reports whether the runtime's session is connected and has
// converged on its plan.
func sessionReconciled(ctx context.Context, rt *goruntime.Runtime, sessionID string) bool {
	for _, s := range rt.DeepHealth(ctx).Sessions {
		if s.SessionID == sessionID {
			return s.Connected && s.SubscriptionsSatisfied != nil && *s.SubscriptionsSatisfied
		}
	}
	return false
}

// sessionRebuilds sums MetricSessionRebuilds for one session.
func sessionRebuilds(metrics *ports.RecordingExporter, sessionID string) int64 {
	var total int64
	for _, entry := range metrics.FindEntries(shared.MetricSessionRebuilds) {
		for _, tag := range entry.Tags {
			if tag.Key == shared.TagKeySessionID && tag.Value == sessionID {
				total += entry.IValue
			}
		}
	}
	return total
}

// sessionCountingMQTTFactory is the real MQTT transport, counting the sessions
// it builds per session id.
type sessionCountingMQTTFactory struct {
	*paho.Factory

	mu    sync.Mutex
	built map[string]int
}

func (f *sessionCountingMQTTFactory) NewSession(ctx context.Context, spec ports.SessionSpec) (ports.Session, error) {
	sess, err := f.Factory.NewSession(ctx, spec)
	if err == nil {
		f.mu.Lock()
		f.built[spec.ID]++
		f.mu.Unlock()
	}
	return sess, err
}

func (f *sessionCountingMQTTFactory) sessionsBuilt(sessionID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.built[sessionID]
}

var _ ports.TransportFactory = (*sessionCountingMQTTFactory)(nil)

type rebuildSinkConfig struct{}

func (rebuildSinkConfig) Kind() string    { return "sink" }
func (rebuildSinkConfig) Validate() error { return nil }

type rebuildSinkRecord struct {
	senderID string
	instance int
	address  string
	payload  string
}

// rebuildSink is the destination transport of both routes. It numbers the
// senders it builds per sender id and records every message they accept. The
// first victim sender holds the first message it is handed until the send's
// context ends, as a destination that stops answering would; everything else
// is accepted at once.
type rebuildSink struct {
	held chan struct{}

	mu       sync.Mutex
	built    map[string]int
	records  []rebuildSinkRecord
	heldBody string
	heldOnce sync.Once
}

func newRebuildSink() *rebuildSink {
	return &rebuildSink{held: make(chan struct{}), built: make(map[string]int)}
}

func (s *rebuildSink) NewSession(context.Context, ports.SessionSpec) (ports.Session, error) {
	return nil, errors.New("sink: sessions are not supported")
}

func (s *rebuildSink) NewReceiver(context.Context, ports.ReceiverSpec, ports.Session) (ports.Receiver, error) {
	return nil, errors.New("sink: receivers are not supported")
}

func (s *rebuildSink) NewSender(_ context.Context, spec ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.built[spec.ID]++
	instance := s.built[spec.ID]
	return &rebuildSinkSender{
		sink:      s,
		senderID:  spec.ID,
		instance:  instance,
		holdFirst: spec.ID == rebuildVictimSenderID && instance == 1,
	}, nil
}

func (s *rebuildSink) Capabilities() []ports.Capability { return nil }

func (s *rebuildSink) AddressValidator() ports.AddressValidator { return nil }

func (s *rebuildSink) hold(payload string) {
	s.heldOnce.Do(func() {
		s.mu.Lock()
		s.heldBody = payload
		s.mu.Unlock()
		close(s.held)
	})
}

func (s *rebuildSink) heldPayload() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heldBody
}

func (s *rebuildSink) record(r rebuildSinkRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
}

func (s *rebuildSink) instances(senderID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.built[senderID]
}

// recorded reports whether exactly sender instance took payload.
func (s *rebuildSink) recorded(senderID string, instance int, payload string) bool {
	return s.find(func(r rebuildSinkRecord) bool {
		return r.senderID == senderID && r.instance == instance && r.payload == payload
	})
}

// recordedFrom reports whether a sender instance numbered minInstance or later
// took payload at the binding's address.
func (s *rebuildSink) recordedFrom(senderID string, minInstance int, payload string) bool {
	return s.find(func(r rebuildSinkRecord) bool {
		return r.senderID == senderID && r.instance >= minInstance && r.payload == payload && r.address == "victim-sink"
	})
}

func (s *rebuildSink) find(match func(rebuildSinkRecord) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if match(r) {
			return true
		}
	}
	return false
}

var _ ports.TransportFactory = (*rebuildSink)(nil)

type rebuildSinkSender struct {
	sink      *rebuildSink
	senderID  string
	instance  int
	holdFirst bool

	mu    sync.Mutex
	sends int
}

func (s *rebuildSinkSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	payload := string(msg.Envelope.Payload())
	s.mu.Lock()
	s.sends++
	first := s.sends == 1
	s.mu.Unlock()
	if s.holdFirst && first {
		s.sink.hold(payload)
		<-ctx.Done()
		return ctx.Err()
	}
	s.sink.record(rebuildSinkRecord{senderID: s.senderID, instance: s.instance, address: msg.Address, payload: payload})
	return nil
}

var _ ports.Sender = (*rebuildSinkSender)(nil)

// lockedLogBuffer collects the bridge log for a failure report. Goroutines may
// still write to it while the test reads it.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
