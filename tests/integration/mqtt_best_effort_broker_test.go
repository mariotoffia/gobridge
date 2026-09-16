package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	runtimesession "github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestMQTTBestEffortBroker_MixedToSQS(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	for _, qos := range []byte{1, 2} {
		t.Run(fmt.Sprintf("qos=0+%d", qos), func(t *testing.T) {
			queue, client := setupSQSQueue(t, "mixed-qos")
			t.Setenv("AWS_ACCESS_KEY_ID", "test")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
			reg := ports.NewRegistry()
			require.NoError(t, paho.Register(reg))
			require.NoError(t, sqs.Register(reg))
			require.NoError(t, nativestore.Register(reg))
			doc, err := os.ReadFile("../../docs/scenarios/24-mqtt-mixed-qos-to-sqs.md")
			require.NoError(t, err)
			_, rest, found := strings.Cut(string(doc), "```yaml\n")
			require.True(t, found)
			yaml, _, found := strings.Cut(rest, "\n```")
			require.True(t, found)
			cfg, err := cfgparser.Parse(strings.NewReader(yaml), cfgparser.FormatYAML, reg)
			require.NoError(t, err)
			sessionCfg, ok := cfg.Sessions[0].Config.(*paho.Config)
			require.True(t, ok)
			sessionCfg.Session.BrokerURL = brokerURL
			sessionCfg.Session.BrokerURLs = []string{brokerURL}
			sessionCfg.Session.ClientID = mqttlocal.UniqueClientID("mixed-qos")
			topic := mqttlocal.UniqueClientID("mixed-topics")
			cfg.Receivers[0].Topics = []ports.SubscriptionDef{{Topic: topic + "/alarms", QoS: int(qos)}, {Topic: topic + "/readings", QoS: 0}}
			cfg.Senders[0].SetDecoded(sqsSenderOpts(queue, flocilocal.Endpoint(t)), nil)
			cfg.Bindings[0].Address = queue
			dir, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			for name, store := range map[string]*ports.StoreConfig{
				"managed": cfg.Stores.ManagedSubscriptions, "dlq": cfg.Stores.DLQ,
			} {
				parent := filepath.Join(dir, name)
				require.NoError(t, os.MkdirAll(parent, 0o700))
				store.SetDecoded(nativestore.SQLiteConfig{Path: filepath.Join(parent, "store.db")}, nil)
			}
			newBuilder := func() *bridge.Builder {
				return bridge.NewBuilder(cfg, bridge.WithBlueprintValidator(config.Validate)).
					RegisterTransportFactory("mqtt", paho.NewFactory(nil)).
					RegisterTransportFactory("sqs", sqs.NewFactory(nil)).
					RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory())
			}
			require.NoError(t, newBuilder().SeedManagedSubscriptionBaselines(t.Context(), map[string][]string{"mqtt-ingress": {}}))
			rt, err := newBuilder().Build(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, rt.Stop(context.Background())) })
			require.NoError(t, rt.Start(t.Context()))
			wait.Until(t, 30*time.Second, "mixed-QoS bridge ready", func() bool { return rt.ReadinessLevel(t.Context()) == ports.LevelFull })
			publishRawMQTT(t, brokerURL, mqttlocal.UniqueClientID("mixed-publisher"),
				&pahov5.Publish{Topic: topic + "/alarms", QoS: qos, Payload: []byte("alarm"),
					Properties: &pahov5.PublishProperties{User: pahov5.UserProperties{{Key: paho.HeaderMessageID, Value: "alarm"}}}},
				&pahov5.Publish{Topic: topic + "/readings", QoS: 0, Payload: []byte("reading")},
			)
			assert.ElementsMatch(t, []string{"alarm", "reading"}, pollSQS(t, client, queue, 2, 30*time.Second))
		})
	}
}

func TestMQTTBestEffortBroker_PublisherZeroThroughSubscriptionOne(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	for _, sink := range []string{"DLQ", "drop", "failed DLQ"} {
		t.Run(sink, func(t *testing.T) {
			metrics := &ports.RecordingExporter{}
			hook := &bestEffortHook{}
			clientID := mqttlocal.UniqueClientID("publisher-zero")
			topic := mqttlocal.UniqueClientID("publisher-zero-topic")
			sess := paho.NewSession(paho.SessionOptions{
				BrokerURLs: []string{brokerURL}, ClientID: clientID, SessionExpiryInterval: 600,
				ConnectTimeout: 5 * time.Second, ReconcileTimeout: 5 * time.Second,
			}, connectivity.SessionPersistent, nil, metrics)
			receiver := paho.NewReceiver("receiver", sess, paho.WithTopicFilters(topic))
			store := &bestEffortDLQ{}
			if sink == "failed DLQ" {
				store.failure = shared.ErrUnavailable
			}
			opts := []goruntime.Option{goruntime.WithMetrics(metrics), goruntime.WithDeliveryHook(hook)}
			if sink != "drop" {
				opts = append(opts, goruntime.WithDLQStore(store))
			}
			rt := goruntime.New(opts...)
			sessionCfg := runtimesession.DefaultConfig(clientID, false)
			sessionCfg.Plan = connectivity.SessionPlan{
				Subscriptions:       []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
				ExpectedReceiverIDs: []string{"receiver"},
			}
			var sends atomic.Int32
			require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
				ID: "publisher-zero", SourceCapabilities: directHoldCaps,
				Policy: routing.RoutePolicy{
					DeliveryMode: routing.DeliveryDirectHold, MaxInFlight: 1, AllowRetryDrop: true,
					OnPermanentFailure: routing.FailureDrop, OnExpired: routing.ExpiredDrop,
				},
			}, receiver, bestEffortSender(func(context.Context, ports.OutboundMessage) error {
				if sends.Add(1) == 1 {
					return shared.ErrUnavailable
				}
				return nil
			}), sess, &sessionCfg))
			t.Cleanup(func() { assert.NoError(t, rt.Stop(context.Background())) })
			require.NoError(t, rt.Start(t.Context()))
			wait.Until(t, 30*time.Second, "QoS 1 subscription ready", func() bool { return rt.ReadinessLevel(t.Context()) == ports.LevelFull })
			for _, id := range []string{"failed", "following"} {
				publishRawMQTT(t, brokerURL, mqttlocal.UniqueClientID("zero-publisher"),
					&pahov5.Publish{Topic: topic, QoS: 0, Payload: []byte(id),
						Properties: &pahov5.PublishProperties{User: pahov5.UserProperties{{Key: paho.HeaderMessageID, Value: id}}}})
			}
			wait.Until(t, 15*time.Second, "both publisher-zero outcomes", func() bool { return len(hook.settled()) == 2 })
			require.NoError(t, rt.WaitQuiescent(t.Context(), goruntime.QuiescenceOptions{Timeout: 5 * time.Second}))
			bestEffortMetric(t, metrics, shared.MetricMessagesSent, 1, "")
			switch sink {
			case "DLQ":
				bestEffortMetric(t, metrics, shared.MetricDLQEntries, 1, "")
				bestEffortMetric(t, metrics, shared.MetricMessagesDropped, 0, "")
			case "drop":
				bestEffortMetric(t, metrics, shared.MetricMessagesDropped, 1, "retry_unsupported")
			default:
				bestEffortMetric(t, metrics, shared.MetricMessagesDropped, 1, "retry_unsupported_dlq_failed")
				bestEffortMetric(t, metrics, shared.MetricDLQEntries, 0, "")
				assert.EqualValues(t, 2, store.writes.Load())
			}
			for _, outcome := range hook.settled() {
				assert.Equal(t, 0, outcome.Envelope.Headers()[paho.HeaderMQTTQoS])
			}
			assert.EqualValues(t, 0, sess.Health(t.Context()).RecoveryRecycleCount)
			assert.EqualValues(t, 0, sess.Health(t.Context()).UnsettledCount)
			assert.Equal(t, ports.ServiceLevelFull, sess.Health(t.Context()).ServiceLevel)
			assert.Empty(t, metrics.FindEntries(paho.MetricMQTTReceiverEmitRejected))
		})
	}
}
