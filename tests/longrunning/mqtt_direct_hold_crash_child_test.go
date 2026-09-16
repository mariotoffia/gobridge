//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

const (
	mqttCrashChildEnv  = "GOBRIDGE_MQTT_CRASH_CHILD"
	mqttCrashConfigEnv = "GOBRIDGE_MQTT_CRASH_CONFIG"
	mqttCrashHoldEnv   = "GOBRIDGE_MQTT_CRASH_HOLD"
)

func TestMQTTDirectHoldCrashChild(t *testing.T) {
	if os.Getenv(mqttCrashChildEnv) != "1" {
		t.Skip("child entrypoint launched by the process harness")
	}
	builder := mqttCrashBuilder(t, requiredCrashEnv(t, mqttCrashConfigEnv), os.Getenv(mqttCrashHoldEnv) == "1")
	rt, err := builder.Build(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Stop(context.Background())) })
	require.NoError(t, rt.Start(t.Context()))
	wait.Until(t, 30*time.Second, "child bridge Full readiness", func() bool {
		return rt.ReadinessLevel(t.Context()) == ports.LevelFull
	})
	fmt.Fprintln(os.Stdout, "MQTT_READY")
	<-t.Context().Done()
}

func mqttCrashBuilder(t *testing.T, path string, hold bool) *bridge.Builder {
	t.Helper()
	reg := ports.NewRegistry()
	require.NoError(t, paho.Register(reg))
	require.NoError(t, sqs.Register(reg))
	require.NoError(t, nativestore.Register(reg))
	cfg, err := cfgparser.ParseFile(path, cfgparser.FormatYAML, reg)
	require.NoError(t, err)
	return bridge.NewBuilder(cfg,
		bridge.WithBlueprintValidator(config.Validate),
		bridge.WithLogger(testLogger(t)),
		bridge.WithMetrics(&mqttCrashMetrics{})).
		RegisterTransportFactory("mqtt", paho.NewFactory(testLogger(t))).
		RegisterTransportFactory("sqs", mqttCrashTargetFactory{TransportFactory: sqs.NewFactory(testLogger(t)), hold: hold}).
		RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory())
}

type mqttCrashTargetFactory struct {
	ports.TransportFactory
	hold bool
}

func (f mqttCrashTargetFactory) NewSender(ctx context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	sender, err := f.TransportFactory.NewSender(ctx, spec, session)
	if err != nil {
		return nil, err
	}
	return mqttCrashSender{inner: sender, hold: f.hold}, nil
}

type mqttCrashSender struct {
	inner ports.Sender
	hold  bool
}

func (s mqttCrashSender) Send(ctx context.Context, message ports.OutboundMessage) error {
	if s.hold {
		fmt.Fprintf(os.Stdout, "MQTT_HELD:%s\n", message.Envelope.ID())
		<-ctx.Done()
		return ctx.Err()
	}
	if err := s.inner.Send(ctx, message); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "MQTT_ACCEPTED:%s\n", message.Envelope.ID())
	return nil
}

type mqttCrashMetrics struct{ ports.NoopExporter }

func (*mqttCrashMetrics) Counter(name string, value int64, _ ...shared.Tag) {
	if name == shared.MetricMessagesDropped || name == shared.MetricDLQEntries {
		fmt.Fprintf(os.Stdout, "MQTT_TERMINAL:%s:%d\n", name, value)
	}
}

var _ ports.TransportFactory = mqttCrashTargetFactory{}
var _ ports.Sender = mqttCrashSender{}
var _ ports.MetricsExporter = (*mqttCrashMetrics)(nil)
