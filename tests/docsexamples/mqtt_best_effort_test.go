package docsexamples_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/runtime"
)

const mqttBestEffortBlueprint = `
bridge:
  id: mixed-qos
stores:
  managed_subscriptions:
    type: sqlite
    options: {path: /var/lib/gobridge/managed/history.db}
  dlq:
    type: sqlite
    options: {path: /var/lib/gobridge/dlq/messages.db}
sessions:
  - id: ingress
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_url: tcp://localhost:1883
        client_id: mixed-qos
        clean_start: false
        session_expiry_interval: 600
receivers:
  - id: receiver
    session_id: ingress
    topics:
      - {topic: "readings/#", qos: 0}
      - {topic: "alarms/#", qos: 1}
senders:
  - id: sender
    transport: sqs
    options:
      queue_url: https://sqs.us-west-1.amazonaws.com/123456789012/events
      region: us-west-1
bindings:
  - {id: destination, sender_id: sender, address: events}
routes:
  - id: route
    receiver_id: receiver
    delivery_mode: direct_hold
    bindings: [destination]
    policy:
      allow_unfenced: true
`

func TestMQTTBestEffortBuilder_EffectiveSessions(t *testing.T) {
	for _, mode := range []string{"ephemeral", "persistent", "exclusive", ""} {
		for _, clean := range []bool{false, true} {
			for _, expiry := range []int{0, 600} {
				for _, qos := range []int{0, 1, 2} {
					for _, alias := range []string{"mqtt", "mqtt.paho"} {
						name := fmt.Sprintf("%s/clean=%t/expiry=%d/qos=0+%d/%s", mode, clean, expiry, qos, alias)
						t.Run(name, func(t *testing.T) {
							text := strings.NewReplacer(
								"transport: mqtt", "transport: "+alias,
								"session_mode: persistent", fmt.Sprintf("session_mode: %q", mode),
								"clean_start: false", fmt.Sprintf("clean_start: %t", clean),
								"session_expiry_interval: 600", fmt.Sprintf("session_expiry_interval: %d", expiry),
								`"alarms/#", qos: 1`, fmt.Sprintf(`"alarms/#", qos: %d`, qos),
							).Replace(mqttBestEffortBlueprint)
							cfg, err := cfgparser.Parse(strings.NewReader(text), cfgparser.FormatYAML, newFullRegistry(t))
							require.NoError(t, err)
							redirectFileStores(t, cfg, realTempDir(t))
							rt, err := newExampleBuilder(t, cfg).
								RegisterTransportFactory("mqtt.paho", paho.NewFactory(nil)).
								Build(t.Context())
							if rt != nil {
								t.Cleanup(func() { assert.NoError(t, rt.Stop(context.Background())) })
							}

							switch {
							case mode == "exclusive":
								require.Error(t, err)
								assert.Contains(t, err.Error(), "exclusive")
							case qos != 0 && (mode != "persistent" || clean):
								require.Error(t, err)
								assert.Contains(t, err.Error(), "ingress session")
								assert.Contains(t, err.Error(), "clean_start")
								assert.NotContains(t, err.Error(), "is QoS 0")
							default:
								require.NoError(t, err)
							}
						})
					}
				}
			}
		}
	}
}

func TestMQTTBestEffortScenario_ActualExamples(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "docs/scenarios/24-mqtt-mixed-qos-to-sqs.md"))
	require.NoError(t, err)
	var accepted, rejected int
	for _, block := range extractYAMLBlocks(string(data)) {
		if !isCompleteBridgeConfig(block.body) {
			continue
		}
		t.Run(block.heading, func(t *testing.T) {
			cfg, err := cfgparser.Parse(strings.NewReader(block.body), cfgparser.FormatYAML, newFullRegistry(t))
			require.NoError(t, err)
			redirectFileStores(t, cfg, realTempDir(t))
			rt, err := newExampleBuilder(t, cfg).Build(t.Context())
			if block.skip {
				rejected++
				require.Error(t, err)
				var validation *runtime.ValidationError
				require.ErrorAs(t, err, &validation)
				assert.Contains(t, err.Error(), "ingress session")
				assert.Contains(t, err.Error(), "clean_start")
				assert.NotContains(t, err.Error(), "is QoS 0")
				assert.Contains(t, string(data), err.Error(), "the scenario must quote the actual rejection")
				assert.Nil(t, rt)
				return
			}
			accepted++
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, rt.Stop(context.Background())) })
		})
	}
	assert.Equal(t, 2, accepted)
	assert.Equal(t, 1, rejected)
}

func TestMQTTBestEffortBuilder_InvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"missing session", "session_id: ingress", "session_id: absent"},
		{"missing config", "client_id: mixed-qos", "client_id: ''"},
		{"empty subscriptions", "topics:\n      - {topic: \"readings/#\", qos: 0}\n      - {topic: \"alarms/#\", qos: 1}", "topics: []"},
		{"empty filter", `"readings/#"`, `""`},
		{"invalid filter", `"readings/#"`, `"readings/#/bad"`},
		{"negative QoS", "qos: 0", "qos: -1"},
		{"QoS above maximum", "qos: 0", "qos: 3"},
		{"QoS wraps to zero", "qos: 0", "qos: 4"},
		{"unknown mode", "session_mode: persistent", "session_mode: invalid"},
		{"fanout", "delivery_mode: direct_hold", "delivery_mode: direct_hold\n    dispatch_mode: fan_out"},
		{"ownership", "allow_unfenced: true", "allow_unfenced: false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.ReplaceAll(mqttBestEffortBlueprint, tc.from, tc.to)
			require.NotEqual(t, mqttBestEffortBlueprint, text)
			cfg, err := cfgparser.Parse(strings.NewReader(text), cfgparser.FormatYAML, newFullRegistry(t))
			if err != nil {
				return
			}
			redirectFileStores(t, cfg, realTempDir(t))
			rt, err := newExampleBuilder(t, cfg).Build(t.Context())
			if rt != nil {
				t.Cleanup(func() { assert.NoError(t, rt.Stop(context.Background())) })
			}
			require.Error(t, err)
		})
	}
}

func TestMQTTBestEffortBuilder_DurableStrongNeedsNoRetryDrop(t *testing.T) {
	for _, qos := range []int{1, 2} {
		for _, expiry := range []int{0, 600} {
			t.Run(fmt.Sprintf("qos=%d/expiry=%d", qos, expiry), func(t *testing.T) {
				text := strings.NewReplacer(
					"qos: 0", fmt.Sprintf("qos: %d", qos),
					"qos: 1", fmt.Sprintf("qos: %d", qos),
					"session_expiry_interval: 600", fmt.Sprintf("session_expiry_interval: %d", expiry),
				).Replace(mqttBestEffortBlueprint)
				text += "      on_permanent_failure: drop\n      on_expired: drop\n"
				cfg, err := cfgparser.Parse(strings.NewReader(text), cfgparser.FormatYAML, newFullRegistry(t))
				require.NoError(t, err)
				cfg.Stores.DLQ = nil
				redirectFileStores(t, cfg, realTempDir(t))
				rt, err := newExampleBuilder(t, cfg).Build(t.Context())
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, rt.Stop(context.Background())) })
			})
		}
	}
}

func TestMQTTBestEffortBuilder_DurableHistoryStillRequired(t *testing.T) {
	cfg, err := cfgparser.Parse(strings.NewReader(mqttBestEffortBlueprint), cfgparser.FormatYAML, newFullRegistry(t))
	require.NoError(t, err)
	cfg.Stores.ManagedSubscriptions = nil
	redirectFileStores(t, cfg, realTempDir(t))
	rt, err := newExampleBuilder(t, cfg).Build(t.Context())
	require.Error(t, err)
	assert.Nil(t, rt)
	assert.Contains(t, err.Error(), "managed_subscriptions")
}

func TestMQTTBestEffortBuilder_FailureSinks(t *testing.T) {
	for _, qos := range []int{0, 1, 2} {
		for _, tc := range []struct {
			name   string
			policy string
			want   string
		}{
			{"implicit drop refused", "on_permanent_failure: drop\n      on_expired: drop", "AllowRetryDrop"},
			{"explicit drop", "allow_retry_drop: true\n      on_permanent_failure: drop\n      on_expired: drop", ""},
			{"permanent DLQ requires store", "allow_retry_drop: true\n      on_expired: drop", "on_permanent_failure=dlq"},
			{"expired DLQ requires store", "allow_retry_drop: true\n      on_permanent_failure: drop", "on_expired=dlq"},
			{"filtered DLQ requires store", "allow_retry_drop: true\n      on_permanent_failure: drop\n      on_expired: drop\n      on_filtered: dlq", "on_filtered=dlq"},
		} {
			t.Run(fmt.Sprintf("qos=0+%d/%s", qos, tc.name), func(t *testing.T) {
				text := strings.ReplaceAll(mqttBestEffortBlueprint, `"alarms/#", qos: 1`, fmt.Sprintf(`"alarms/#", qos: %d`, qos))
				text += "      " + tc.policy + "\n"
				cfg, err := cfgparser.Parse(strings.NewReader(text), cfgparser.FormatYAML, newFullRegistry(t))
				require.NoError(t, err)
				cfg.Stores.DLQ = nil
				redirectFileStores(t, cfg, realTempDir(t))
				rt, err := newExampleBuilder(t, cfg).Build(t.Context())
				if rt != nil {
					t.Cleanup(func() { assert.NoError(t, rt.Stop(context.Background())) })
				}
				if tc.want == "" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tc.want)
				}
			})
		}
	}
}
