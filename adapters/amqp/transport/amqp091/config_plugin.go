package amqp091

import (
	"errors"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contract.
var _ ports.CredentialedConfig = (*Config)(nil)

// Config is the typed PluginConfig for the AMQP 0-9-1 (RabbitMQ)
// transport. It nests session/receiver/sender role configs and is
// shared across SessionSpec.Config / ReceiverSpec.Config /
// SenderSpec.Config. Per-topic Subscription/Publisher knobs are
// nested so a single decoder produces a Config that can populate a
// SubscriptionPlan.Config or a binding.
type Config struct {
	Session      SessionOptions      `mapstructure:"session" yaml:"session" json:"session"`
	Receiver     ReceiverParams      `mapstructure:"receiver" yaml:"receiver" json:"receiver"`
	Sender       SenderParams        `mapstructure:"sender" yaml:"sender" json:"sender"`
	Subscription SubscriptionParams  `mapstructure:"subscription" yaml:"subscription" json:"subscription"`
	Publisher    PublisherParams     `mapstructure:"publisher" yaml:"publisher" json:"publisher"`

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

// SenderParams holds user-settable sender fields.
type SenderParams struct {
	Exchange   string        `mapstructure:"exchange" yaml:"exchange" json:"exchange"`
	RoutingKey string        `mapstructure:"routing_key" yaml:"routing_key" json:"routing_key"`
	Mandatory  bool          `mapstructure:"mandatory" yaml:"mandatory" json:"mandatory"`
	Immediate  bool          `mapstructure:"immediate" yaml:"immediate" json:"immediate"`
	Timeout    time.Duration `mapstructure:"timeout" yaml:"timeout" json:"timeout"`
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
}

// PublisherParams describes per-binding publisher setup performed
// when declaring an outbound publisher. Mirrors SubscriptionParams.
type PublisherParams struct {
	Exchange     string `mapstructure:"exchange" yaml:"exchange" json:"exchange"`
	RoutingKey   string `mapstructure:"routing_key" yaml:"routing_key" json:"routing_key"`
	ExchangeType string `mapstructure:"exchange_type" yaml:"exchange_type" json:"exchange_type"`
	Durable      bool   `mapstructure:"durable" yaml:"durable" json:"durable"`
	AutoDelete   bool   `mapstructure:"auto_delete" yaml:"auto_delete" json:"auto_delete"`
}

// Kind reports the registry discriminator.
func (Config) Kind() string { return "amqp.amqp091" }

// Validate checks the unified config. Empty role-specific fields are
// allowed because the same Config is reused across all three specs
// and not all roles are populated for each spec.
func (c Config) Validate() error {
	if c.Session.BrokerURL == "" && c.Receiver.QueueName == "" && c.Sender.Exchange == "" &&
		c.Subscription.Exchange == "" && c.Publisher.Exchange == "" {
		return errors.New("amqp091: at least one of session.broker_url, receiver.queue_name, sender.exchange, subscription.exchange, or publisher.exchange must be set")
	}
	if c.Session.BrokerURL != "" {
		if err := c.Session.validate(); err != nil {
			return err
		}
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
func (c *Config) ApplyCredentials(set *domain.CredentialSet) error {
	if c == nil {
		return errors.New("amqp091: nil config")
	}
	if set == nil {
		c.CredentialsURIRef = ""
		return nil
	}
	if set.Password != nil {
		if c.Session.Username == "" {
			c.Session.Username = set.Password.Username
		}
		if c.Session.Password == "" {
			c.Session.Password = set.Password.Password
		}
	}
	if set.TLS != nil {
		applyAMQPTLSMaterial(&c.Session.TLS, set.TLS)
	}
	c.CredentialsURIRef = ""
	return nil
}
