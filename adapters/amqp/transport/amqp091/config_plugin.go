package amqp091

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contract.
var _ ports.CredentialedConfig = (*Config)(nil)
var _ ports.FreezableConfig = Config{}
var _ ports.PublishingConfig = (*Config)(nil)

// Config is the typed PluginConfig for the AMQP 0-9-1 (RabbitMQ)
// transport. It nests session/receiver/sender role configs and is
// shared across SessionSpec.Config / ReceiverSpec.Config /
// SenderSpec.Config. Per-topic Subscription/Publisher knobs are
// nested so a single decoder produces a Config that can populate a
// SubscriptionPlan.Config or a binding.
type Config struct {
	Session      SessionOptions     `mapstructure:"session" yaml:"session" json:"session"`
	Receiver     ReceiverParams     `mapstructure:"receiver" yaml:"receiver" json:"receiver"`
	Sender       SenderParams       `mapstructure:"sender" yaml:"sender" json:"sender"`
	Subscription SubscriptionParams `mapstructure:"subscription" yaml:"subscription" json:"subscription"`
	Publisher    PublisherParams    `mapstructure:"publisher" yaml:"publisher" json:"publisher"`

	// CredentialsURIRef is the optional URI consulted by the bridge's
	// credential store at build time. The resolved material is
	// applied via ApplyCredentials.
	CredentialsURIRef string `mapstructure:"credentials_uri" yaml:"credentials_uri,omitempty" json:"credentials_uri,omitempty"`
}

// ReceiverParams holds user-settable receiver fields.
type ReceiverParams struct {
	QueueName     string `mapstructure:"queue_name" yaml:"queue_name" json:"queue_name"`
	ConsumerTag   string `mapstructure:"consumer_tag" yaml:"consumer_tag" json:"consumer_tag"`
	AutoAck       bool   `mapstructure:"auto_ack" yaml:"auto_ack" json:"auto_ack"`
	Exclusive     bool   `mapstructure:"exclusive" yaml:"exclusive" json:"exclusive"`
	PrefetchCount int    `mapstructure:"prefetch_count" yaml:"prefetch_count" json:"prefetch_count"`
	PrefetchSize  int    `mapstructure:"prefetch_size" yaml:"prefetch_size" json:"prefetch_size"`
}

// defaultPrefetchCount is the QoS prefetch applied when a receiver config
// omits prefetch_count. It bounds the number of unacknowledged deliveries
// the broker pushes to a single consumer.
const defaultPrefetchCount = 10

// applyDefaults fills receiver defaults that a typed (struct) config loses
// when a field is omitted. A zero prefetch_count is treated as the safe
// default rather than "unlimited prefetch": with manual settlement an
// unbounded window lets the broker hand the whole queue to one consumer,
// exhausting memory and defeating fair dispatch. Operators who genuinely
// want a large window must set an explicit positive prefetch_count.
//
// ponytail: 0 maps to the bounded default (not unlimited) deliberately;
// unlimited prefetch is the backpressure footgun this default exists to
// prevent.
func (p *ReceiverParams) applyDefaults() {
	if p.PrefetchCount == 0 {
		p.PrefetchCount = defaultPrefetchCount
	}
}

// SenderParams holds user-settable sender fields.
type SenderParams struct {
	Exchange   string `mapstructure:"exchange" yaml:"exchange" json:"exchange"`
	RoutingKey string `mapstructure:"routing_key" yaml:"routing_key" json:"routing_key"`
	Mandatory  bool   `mapstructure:"mandatory" yaml:"mandatory" json:"mandatory"`
	Immediate  bool   `mapstructure:"immediate" yaml:"immediate" json:"immediate"`
	// AllowUnroutableDrop is the explicit opt-in that lets a managed sender
	// publish with mandatory=false. With mandatory=false the broker CONFIRMS
	// an unroutable publish and then silently DISCARDS it, so the bridge acks
	// the source and the message is lost with zero telemetry (wrong routing
	// key / missing binding after a deploy). The managed factory therefore
	// refuses to build a sender unless it is mandatory OR this flag admits the
	// silent-drop behaviour deliberately (throughput-over-safety fan-out where
	// unroutable is expected). It never changes the publish itself — it only
	// records that the operator accepts the loss.
	AllowUnroutableDrop bool `mapstructure:"allow_unroutable_drop" yaml:"allow_unroutable_drop" json:"allow_unroutable_drop"`
	// DeliveryMode selects the default persistence of every publish:
	// "persistent" (default; AMQP delivery-mode 2, survives a broker
	// restart on a durable queue) or "transient" (delivery-mode 1, lost
	// on broker restart even on a durable classic queue). A per-message
	// "amqp091.delivery-mode" envelope header overrides it. Quorum
	// queues persist regardless of this knob.
	DeliveryMode string        `mapstructure:"delivery_mode" yaml:"delivery_mode" json:"delivery_mode"`
	Timeout      time.Duration `mapstructure:"timeout" yaml:"timeout" json:"timeout"`
}

// SubscriptionParams describes per-topic broker setup performed when
// declaring an inbound subscription. These values previously lived in
// SubscriptionPlan.Options; PHASE2 carries them through the typed
// Config attached to SubscriptionPlan.Config.
type SubscriptionParams struct {
	Exchange     string `mapstructure:"exchange" yaml:"exchange" json:"exchange"`
	RoutingKey   string `mapstructure:"routing_key" yaml:"routing_key" json:"routing_key"`
	ExchangeType string `mapstructure:"exchange_type" yaml:"exchange_type" json:"exchange_type"`
	Durable      bool   `mapstructure:"durable" yaml:"durable" json:"durable"`
	AutoDelete   bool   `mapstructure:"auto_delete" yaml:"auto_delete" json:"auto_delete"`
	// QueueArguments are passed verbatim as the AMQP queue-declare
	// arguments table (e.g. x-queue-type=quorum, x-dead-letter-exchange,
	// x-message-ttl, x-max-length). This is what enables quorum queues,
	// dead-lettering, TTL and length limits. Numeric values should be
	// integers — RabbitMQ rejects a float where it expects an integer.
	QueueArguments map[string]any `mapstructure:"queue_arguments" yaml:"queue_arguments" json:"queue_arguments"`
	// ExchangeArguments are passed as the exchange-declare arguments
	// table (e.g. alternate-exchange, or x-delayed-type for the delayed
	// message exchange plugin).
	ExchangeArguments map[string]any `mapstructure:"exchange_arguments" yaml:"exchange_arguments" json:"exchange_arguments"`
	// BindingArguments are passed as the queue-bind arguments table
	// (e.g. x-match plus headers for a headers-exchange binding).
	BindingArguments map[string]any `mapstructure:"binding_arguments" yaml:"binding_arguments" json:"binding_arguments"`
}

// PublisherParams describes per-binding publisher setup performed
// when declaring an outbound publisher. Mirrors SubscriptionParams.
type PublisherParams struct {
	Exchange     string `mapstructure:"exchange" yaml:"exchange" json:"exchange"`
	RoutingKey   string `mapstructure:"routing_key" yaml:"routing_key" json:"routing_key"`
	ExchangeType string `mapstructure:"exchange_type" yaml:"exchange_type" json:"exchange_type"`
	Durable      bool   `mapstructure:"durable" yaml:"durable" json:"durable"`
	AutoDelete   bool   `mapstructure:"auto_delete" yaml:"auto_delete" json:"auto_delete"`
	// ExchangeArguments are passed as the exchange-declare arguments
	// table (see SubscriptionParams.ExchangeArguments).
	ExchangeArguments map[string]any `mapstructure:"exchange_arguments" yaml:"exchange_arguments" json:"exchange_arguments"`
}

// FreezePluginConfig returns a deep-owned build snapshot. Clock remains shared:
// it is an injected runtime dependency, not mutable configuration data.
func (c Config) FreezePluginConfig() ports.PluginConfig {
	frozen := c
	if c.Session.TLS != nil {
		tlsCopy := *c.Session.TLS
		frozen.Session.TLS = &tlsCopy
	}
	frozen.Subscription.QueueArguments = cloneAMQPArguments(c.Subscription.QueueArguments)
	frozen.Subscription.ExchangeArguments = cloneAMQPArguments(c.Subscription.ExchangeArguments)
	frozen.Subscription.BindingArguments = cloneAMQPArguments(c.Subscription.BindingArguments)
	frozen.Publisher.ExchangeArguments = cloneAMQPArguments(c.Publisher.ExchangeArguments)
	return &frozen
}

func cloneAMQPArguments(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	cloned := make(map[string]any, len(src))
	for key, value := range src {
		cloned[key] = cloneAMQPArgumentValue(value)
	}
	return cloned
}

// cloneAMQPArgumentValue recursively owns every mutable AMQP field form:
// bridge-native maps, SDK Tables, field arrays, and byte arrays. The remaining
// SDK-supported field forms are immutable scalar values and can be shared.
func cloneAMQPArgumentValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAMQPArguments(typed)
	case []any:
		cloned := make([]any, len(typed))
		for i := range typed {
			cloned[i] = cloneAMQPArgumentValue(typed[i])
		}
		return cloned
	case []byte:
		return append([]byte(nil), typed...)
	default:
		if cloned, ok := cloneSDKAMQPArgumentValue(value); ok {
			return cloned
		}
		return value
	}
}

// Kind reports the registry discriminator.
func (Config) Kind() string { return "amqp.amqp091" }

// PublisherTopic returns the exchange this sender publishes to, so the bridge
// can thread it into the session plan and the session's declarePublisher can
// pre-declare it (ports.PublishingConfig). The declared exchange NAME is always
// sourced from sender.exchange (the exact exchange Send publishes to); the
// declaration TOPOLOGY (exchange_type/durable/auto_delete/arguments) is read
// from the separate publisher.* block, which only decorates that exchange —
// publisher.exchange is NOT a second name and is ignored here. Set sender.exchange
// alone and the exchange is declared with defaults (direct, non-durable); add a
// publisher.* block to declare non-default topology. Empty means "no exchange to
// declare" (default-exchange publish); declarePublisher no-ops on an empty topic.
// The declare is best-effort: see Session.reconcile.
func (c Config) PublisherTopic() string { return c.Sender.Exchange }

// PublisherTopologyKey returns a deterministic descriptor of the exchange
// DECLARATION topology this sender contributes (ports.PublishingConfig), so the
// bridge can distinguish a legitimate identical re-declaration of an
// already-advertised exchange from a genuinely DIVERGENT one when it dedups
// senders by exchange name (first-declare-wins) — REV-2-topowarn. It encodes
// EXACTLY the fields declarePublisher passes to ExchangeDeclare: exchange_type
// (default "direct", mirroring publisherParams), durable, auto_delete, and the
// exchange-argument table (keys sorted for determinism), all read from the
// publisher.* block. The routing key is deliberately excluded — it is a
// per-message property, not part of the exchange declaration, so two senders on
// the same exchange with different routing keys do NOT diverge. Two configs
// declaring the same exchange topology therefore yield an identical key.
func (c Config) PublisherTopologyKey() string {
	p := c.Publisher
	exchangeType := p.ExchangeType
	if exchangeType == "" {
		exchangeType = "direct"
	}
	return fmt.Sprintf("type=%s;durable=%t;auto_delete=%t;args=%s",
		exchangeType, p.Durable, p.AutoDelete, stableArgs(p.ExchangeArguments))
}

// stableArgs renders an AMQP argument table as a deterministic string (keys
// sorted) so two tables can be compared for equality regardless of map
// iteration order.
func stableArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%v", k, args[k])
	}
	return b.String()
}

// Validate checks the unified config. Empty role-specific fields are
// allowed because the same Config is reused across all three specs
// and not all roles are populated for each spec.
// Validate checks field ranges and internal consistency. It runs at
// parse time on EVERY attachment point that reuses this Config shape
// (session, receiver, sender, binding override), so it deliberately
// does not require connection or entity identity: a binding carries
// only overrides, and an empty sender.exchange is the default
// exchange. The factory enforces role-specific completeness
// (session.broker_url, receiver queue) at build time.
func (c Config) Validate() error {
	if c.Session.BrokerURL != "" {
		if err := c.Session.validate(); err != nil {
			return err
		}
	}
	if c.Receiver.AutoAck {
		return errors.New("amqp091: receiver.auto_ack=true is unsafe for a managed route: " +
			"the bridge settles each delivery only after the downstream send/persist succeeds, " +
			"whereas broker auto-ack acknowledges on delivery and silently drops messages when a " +
			"downstream step fails. Remove auto_ack — the default (false) provides at-least-once settlement")
	}
	// A negative prefetch skips ch.Qos entirely (QoS is gated on > 0), which
	// disables broker-side flow control: the SDK's consumers.buffer is
	// unbounded, so the broker streams the whole queue to one manual-settle
	// consumer and the process OOMs on a deep queue. 0 is the safe bounded
	// default (see ReceiverParams.applyDefaults); negatives are never valid.
	if c.Receiver.PrefetchCount < 0 {
		return fmt.Errorf("amqp091: receiver.prefetch_count must not be negative (got %d): "+
			"a negative prefetch disables QoS and lets the broker push the entire queue to one "+
			"manual-settlement consumer, exhausting memory. Use 0 for the bounded default or a "+
			"positive window", c.Receiver.PrefetchCount)
	}
	if c.Receiver.PrefetchSize < 0 {
		return fmt.Errorf("amqp091: receiver.prefetch_size must not be negative (got %d)",
			c.Receiver.PrefetchSize)
	}
	if c.Sender.Immediate {
		return errors.New("amqp091: sender.immediate=true is not supported by RabbitMQ: the broker " +
			"removed basic.publish 'immediate' in 3.0 and closes the channel when it is set. Remove it")
	}
	if err := validateDeliveryMode(c.Sender.DeliveryMode); err != nil {
		return fmt.Errorf("amqp091: sender.%w", err)
	}
	// Duration floors run UNCONDITIONALLY (unlike the broker_url gate
	// above): a binding override may carry only timing knobs, and a
	// bare-int decode accident (heartbeat: 30 → 30ns) must be caught on
	// every attachment point. See minConfigDuration.
	if err := c.Session.validateDurations(); err != nil {
		return fmt.Errorf("amqp091: %w", err)
	}
	if err := validateDurationFloor("sender.timeout", c.Sender.Timeout); err != nil {
		return fmt.Errorf("amqp091: %w", err)
	}
	return nil
}

// CredentialsURI implements ports.CredentialedConfig.
func (c *Config) CredentialsURI() string {
	if c == nil {
		return ""
	}
	return c.CredentialsURIRef
}

// ApplyCredentials implements ports.CredentialedConfig. The resolved
// password credential populates Session.Username/Password and the TLS
// material populates Session.TLS PEM fields. Pre-existing inline
// values are left untouched ("config wins" precedence).
func (c *Config) ApplyCredentials(set *connectivity.CredentialSet) error {
	if c == nil {
		return errors.New("amqp091: nil config")
	}
	if set == nil {
		c.CredentialsURIRef = ""
		return nil
	}
	if set.Password() != nil {
		if c.Session.Username == "" {
			c.Session.Username = set.Password().Username()
		}
		if c.Session.Password.IsZero() {
			c.Session.Password = set.Password().Password()
		}
	}
	if set.TLS() != nil {
		applyAMQPTLSMaterial(&c.Session.TLS, set.TLS())
	}
	c.CredentialsURIRef = ""
	return nil
}
