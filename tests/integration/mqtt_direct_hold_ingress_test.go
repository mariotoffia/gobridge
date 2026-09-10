package integration_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// The shape direct_hold exists to enable: an MQTT ingress on a durable session
// holds the broker delivery until SQS has accepted it — no outbox, no lease, no
// outbox partition — with only the managed-subscription store the durable
// session owes the broker. Nothing but the receiver names the ingress session,
// so the receiver's own binding to it is what connects it and reconciles its
// subscriptions.

// historyOnlyStores serves stores.managed_subscriptions from a SQLite file and
// refuses every other role, so the bridge under test cannot quietly acquire a
// lease or an outbox.
type historyOnlyStores struct{ path string }

func (historyOnlyStores) NewLeaseStore(context.Context, ports.PluginConfig) (ports.LeaseStore, error) {
	return nil, errors.New("this bridge holds no lease")
}

func (historyOnlyStores) NewOutboxStore(context.Context, ports.PluginConfig, ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return nil, errors.New("this bridge persists no outbox")
}

func (historyOnlyStores) NewDLQStore(context.Context, ports.PluginConfig) (ports.DLQStore, error) {
	return nil, errors.New("this bridge writes no dead letters")
}

func (f historyOnlyStores) NewManagedSubscriptionStore(ctx context.Context, _ ports.PluginConfig) (ports.ManagedSubscriptionStore, error) {
	return sqlitemanagedsubscriptions.NewStoreContext(ctx, f.path)
}

func (historyOnlyStores) IsCrashDurable() bool { return true }

type historyStoreConfig struct{}

func (historyStoreConfig) Kind() string    { return "sqlite" }
func (historyStoreConfig) Validate() error { return nil }

func TestDirectHoldMQTTIngressOnDurableSessionCarriesMessagesWithoutOutboxOrLease(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	queueURL, sqsClient := setupSQSQueue(t, "direct-hold-ingress")
	sqsEndpoint := flocilocal.Endpoint(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	clientID := mqttlocal.UniqueClientID("direct-hold-ingress")
	topicRoot := mqttlocal.UniqueClientID("direct-hold")
	filter := topicRoot + "/#"
	historyPath := filepath.Join(t.TempDir(), "history", "managed-subscriptions.db")

	sessionConfig := &paho.Config{Session: paho.SessionOptions{
		BrokerURLs: []string{brokerURL}, ClientID: clientID,
		ConnectTimeout: 5 * time.Second, KeepAlive: 10, SessionExpiryInterval: 300,
	}}
	session := ports.SessionDef{ID: "mqtt-in", Transport: "mqtt", SessionMode: string(connectivity.SessionPersistent)}
	session.SetDecoded(sessionConfig, nil)
	receiver := ports.ReceiverDef{
		ID: "mqtt-rx", Transport: "mqtt", SessionID: "mqtt-in",
		Topics: []ports.SubscriptionDef{{Topic: filter, QoS: 1}},
	}
	receiver.SetDecoded(&paho.Config{}, nil)
	senderConfig := sqsSenderOpts(queueURL, sqsEndpoint)
	sender := ports.SenderDef{ID: "sqs-tx", Transport: "sqs"}
	sender.SetDecoded(senderConfig, nil)
	binding := ports.BindingDef{ID: "sqs-out", SenderID: "sqs-tx", Address: queueURL}
	binding.SetDecoded(senderConfig, nil)
	history := &ports.StoreConfig{Type: "sqlite"}
	history.SetDecoded(historyStoreConfig{}, nil)

	cfg := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "direct-hold-ingress"},
		Stores:    ports.StoresConfig{ManagedSubscriptions: history},
		Sessions:  []ports.SessionDef{session},
		Receivers: []ports.ReceiverDef{receiver},
		Senders:   []ports.SenderDef{sender},
		Bindings:  []ports.BindingDef{binding},
		Routes: []ports.RouteDef{{
			ID: "mqtt-to-sqs", ReceiverID: "mqtt-rx", DeliveryMode: "direct_hold",
			Bindings: []string{"sqs-out"},
			// One process, one subscriber: the fence exists for two consumers of
			// one subscription racing the same partition, and there is none.
			Policy: ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop", AllowUnfenced: true},
		}},
	}
	newBuilder := func() *bridge.Builder {
		return bridge.NewBuilder(cfg).
			RegisterTransportFactory("mqtt", paho.NewFactory(nil)).
			RegisterTransportFactory("sqs", sqsadapter.NewFactory(slog.Default())).
			RegisterStoreFactory("sqlite", historyOnlyStores{path: historyPath})
	}

	// The durable session owes the broker an exact filter history and does not
	// start without a baseline; attest a new identity the way a deployment does.
	if err := newBuilder().SeedManagedSubscriptionBaselines(ctx, map[string][]string{"mqtt-in": {}}); err != nil {
		t.Fatalf("seed the managed-subscription baseline: %v", err)
	}
	rt, err := newBuilder().Build(ctx)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	wait.Until(t, 30*time.Second, "the bridge reaches Full readiness on its ingress session", func() bool {
		return rt.ReadinessLevel(ctx) >= ports.LevelFull
	})

	// The shape: a standalone instance whose only session holds no lease, on a
	// route that holds the source instead of copying it.
	if role := rt.Role(); role != ports.RoleStandalone {
		t.Fatalf("role = %q, want %q: nothing here takes a lease", role, ports.RoleStandalone)
	}
	health := rt.DeepHealth(ctx)
	if len(health.Sessions) != 1 || health.Sessions[0].SessionID != "mqtt-in" || health.Sessions[0].HasLease {
		t.Fatalf("deep health sessions = %+v, want only mqtt-in, lease-less", health.Sessions)
	}
	if len(health.Routes) != 1 || health.Routes[0].DeliveryMode != "direct_hold" {
		t.Fatalf("deep health routes = %+v, want mqtt-to-sqs on direct_hold", health.Routes)
	}

	// Messages cross the route.
	publisher := setupMQTTSession(t, mqttlocal.UniqueClientID("direct-hold-publisher"), connectivity.SessionEphemeral)
	publish := paho.NewSender(publisher, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})
	const count = 5
	markers := make([]string, 0, count)
	for i := range count {
		marker := fmt.Sprintf("direct-hold-%s-%d", topicRoot, i)
		markers = append(markers, marker)
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID: marker, Subject: "sensor.reading", Payload: []byte(`{"marker":"` + marker + `"}`),
		})
		if err := publish.Send(ctx, ports.OutboundMessage{Envelope: env, Address: topicRoot + "/sensor"}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	bodies := pollSQS(t, sqsClient, queueURL, count, 30*time.Second)
	for _, marker := range markers {
		if !slices.ContainsFunc(bodies, func(body string) bool { return strings.Contains(body, marker) }) {
			t.Fatalf("message %s never reached the queue; received %d bodies: %v", marker, len(bodies), bodies)
		}
	}

	// The durable session recorded the exact filter it installed in the store
	// on disk, which is what makes a later removal safe (ADR 0003).
	if err := rt.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	identity, err := sessionConfig.DurableSessionIdentity(connectivity.SessionPersistent)
	if err != nil {
		t.Fatalf("derive the durable identity: %v", err)
	}
	store, err := sqlitemanagedsubscriptions.NewStore(historyPath)
	if err != nil {
		t.Fatalf("reopen the managed-subscription store: %v", err)
	}
	t.Cleanup(func() { _ = io.Closer(store).Close() })
	filters, err := store.List(ctx, identity)
	if err != nil {
		t.Fatalf("list the managed history: %v", err)
	}
	if !slices.Contains(filters, filter) {
		t.Fatalf("managed history = %v, want it to hold the installed filter %q", filters, filter)
	}
}
