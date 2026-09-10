// ═══════════════════════════════════════════════
// Production-readiness remediation test: canonical YAML shape.
//
// The published docs showed FLAT option keys (options.broker_url,
// options.queue_name, options.exchange) which the strict (ErrorUnused)
// nested decoder rejects — every documented RabbitMQ example failed to
// boot. This test pins the CANONICAL nested YAML shape (session./
// receiver./sender./subscription./publisher. blocks) end-to-end:
// YAML text → yaml.v3 → parser.NewRawConfig(...).Decode → typed Config,
// i.e. the exact pipeline the runtime uses for a transport `options:`
// block. The docs must be rewritten to match this shape.
// ═══════════════════════════════════════════════
package amqp091

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/mariotoffia/gobridge/config/parser"
)

// canonicalOptionsYAML is a realistic, full RabbitMQ transport options
// block — including auto-declare (subscription/publisher) with quorum
// queue arguments and the x-delivery-limit poison guard. This is the
// shape docs/transport-configuration.md and the RabbitMQ scenarios must
// document. Durations are quoted Go duration strings.
const canonicalOptionsYAML = `
credentials_uri: "aws-secrets://gobridge/rabbit-a"
session:
  broker_url: "amqp://rabbit-a.internal:5672/"
  vhost: "/prod"
  heartbeat: "10s"
  connect_timeout: "10s"
  reconnect_delay: "1s"
  reconnect_max_delay: "30s"
  reconnect_multiplier: 2.0
  tls:
    enable: true
    cert_file: "/etc/gobridge/tls/client.pem"
    key_file: "/etc/gobridge/tls/client.key"
receiver:
  queue_name: "orders.inbound"
  consumer_tag: "gobridge-orders"
  prefetch_count: 64
sender:
  exchange: "orders"
  routing_key: "orders.bridged"
  mandatory: true
  delivery_mode: "persistent"
  timeout: "5s"
subscription:
  exchange: "orders"
  exchange_type: "topic"
  routing_key: "orders.#"
  durable: true
  queue_arguments:
    x-queue-type: "quorum"
    x-delivery-limit: 5
publisher:
  exchange: "orders"
  exchange_type: "topic"
  durable: true
`

// TestPluginOptionsDecode_CanonicalYAML_FullExample pins the canonical
// nested YAML configuration shape through the production decode path.
func TestPluginOptionsDecode_CanonicalYAML_FullExample(t *testing.T) {
	var raw map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(canonicalOptionsYAML), &raw))

	var cfg Config
	require.NoError(t, parser.NewRawConfig(raw).Decode(&cfg),
		"the canonical nested YAML shape must decode through the strict production decoder")

	// Session.
	require.Equal(t, "amqp://rabbit-a.internal:5672/", cfg.Session.BrokerURL)
	require.Equal(t, "/prod", cfg.Session.Vhost)
	require.Equal(t, 10*time.Second, cfg.Session.Heartbeat)
	require.Equal(t, 10*time.Second, cfg.Session.ConnectTimeout)
	require.Equal(t, time.Second, cfg.Session.ReconnectDelay)
	require.Equal(t, 30*time.Second, cfg.Session.ReconnectMaxDelay)
	require.Equal(t, 2.0, cfg.Session.ReconnectMultiplier)
	require.Equal(t, "aws-secrets://gobridge/rabbit-a", cfg.CredentialsURIRef)
	require.NotNil(t, cfg.Session.TLS)
	require.True(t, cfg.Session.TLS.Enable)
	require.Equal(t, "/etc/gobridge/tls/client.pem", cfg.Session.TLS.CertFile)
	require.Equal(t, "/etc/gobridge/tls/client.key", cfg.Session.TLS.KeyFile)

	// Receiver.
	require.Equal(t, "orders.inbound", cfg.Receiver.QueueName)
	require.Equal(t, "gobridge-orders", cfg.Receiver.ConsumerTag)
	require.Equal(t, 64, cfg.Receiver.PrefetchCount)

	// Sender (incl. the delivery_mode knob).
	require.Equal(t, "orders", cfg.Sender.Exchange)
	require.Equal(t, "orders.bridged", cfg.Sender.RoutingKey)
	require.True(t, cfg.Sender.Mandatory)
	require.Equal(t, DeliveryModePersistent, cfg.Sender.DeliveryMode)
	require.Equal(t, 5*time.Second, cfg.Sender.Timeout)

	// Subscription auto-declare (quorum + poison guard).
	require.Equal(t, "orders", cfg.Subscription.Exchange)
	require.Equal(t, "topic", cfg.Subscription.ExchangeType)
	require.Equal(t, "orders.#", cfg.Subscription.RoutingKey)
	require.True(t, cfg.Subscription.Durable)
	require.Equal(t, "quorum", cfg.Subscription.QueueArguments["x-queue-type"])
	require.EqualValues(t, 5, cfg.Subscription.QueueArguments["x-delivery-limit"])

	// Publisher auto-declare.
	require.Equal(t, "orders", cfg.Publisher.Exchange)
	require.Equal(t, "topic", cfg.Publisher.ExchangeType)
	require.True(t, cfg.Publisher.Durable)

	// The decoded config must also pass semantic validation.
	require.NoError(t, cfg.Validate())
}

// TestPluginOptionsDecode_DocumentedFlatShape_IsRejected pins that the
// OLD documented flat shape does NOT decode — it is the docs that must
// change, not the decoder (strictness is a feature: typoed keys fail
// fast instead of silently misconfiguring a bridge).
func TestPluginOptionsDecode_DocumentedFlatShape_IsRejected(t *testing.T) {
	const flatYAML = `
broker_url: "amqp://rabbit-a.internal:5672/"
queue_name: "orders.inbound"
exchange: "orders"
`
	var raw map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(flatYAML), &raw))

	var cfg Config
	err := parser.NewRawConfig(raw).Decode(&cfg)
	require.Error(t, err, "flat option keys must be rejected by the strict nested decoder")
	require.Contains(t, err.Error(), "broker_url")
}
