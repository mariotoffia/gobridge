package amqp10

import (
	"errors"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contract.
var _ ports.CredentialedConfig = (*Config)(nil)

// Config is the typed PluginConfig for the AMQP 1.0 transport. It
// nests session/receiver/sender role configs and is shared across
// SessionSpec.Config / ReceiverSpec.Config / SenderSpec.Config.
type Config struct {
	Session  SessionOptions `mapstructure:"session" yaml:"session" json:"session"`
	Receiver ReceiverParams `mapstructure:"receiver" yaml:"receiver" json:"receiver"`
	Sender   SenderParams   `mapstructure:"sender" yaml:"sender" json:"sender"`

	// CredentialsURIRef is the optional URI consulted by the bridge's
	// credential store at build time. Resolved material is applied
	// via ApplyCredentials.
	CredentialsURIRef string `mapstructure:"credentials_uri" yaml:"credentials_uri,omitempty" json:"credentials_uri,omitempty"`
}

// ReceiverParams holds user-settable receiver fields.
type ReceiverParams struct {
	Address        string      `mapstructure:"address" yaml:"address" json:"address"`
	LinkCredit     uint32      `mapstructure:"link_credit" yaml:"link_credit" json:"link_credit"`
	DurabilityMode uint32      `mapstructure:"durability_mode" yaml:"durability_mode" json:"durability_mode"`
	Routing        RoutingType `mapstructure:"routing" yaml:"routing" json:"routing"`

	// SubscriptionName pins the AMQP link name so a durable
	// subscription (durability_mode > 0) survives reconnects. When
	// empty, a stable name is derived from container_id + address.
	SubscriptionName string `mapstructure:"subscription_name" yaml:"subscription_name,omitempty" json:"subscription_name,omitempty"`
}

// SenderParams holds user-settable sender fields.
type SenderParams struct {
	Address        string        `mapstructure:"address" yaml:"address" json:"address"`
	Timeout        time.Duration `mapstructure:"timeout" yaml:"timeout" json:"timeout"`
	DurabilityMode uint32        `mapstructure:"durability_mode" yaml:"durability_mode" json:"durability_mode"`
	Routing        RoutingType   `mapstructure:"routing" yaml:"routing" json:"routing"`

	// Durable sets the AMQP message header durable flag on outbound
	// messages. Unset defaults to true (persistent); set to false to
	// opt into non-persistent (faster, lost on broker restart) sends.
	Durable *bool `mapstructure:"durable" yaml:"durable,omitempty" json:"durable,omitempty"`
}

// Kind reports the plugin discriminator.
func (Config) Kind() string { return "amqp.amqp10" }

// Validate checks required fields per role. Empty role-specific
// fields are allowed because the same Config is reused across all
// three specs and not all roles are populated for each spec.
// Validate checks field ranges and consistency. It runs at parse time
// on EVERY attachment point that reuses this Config shape (session,
// receiver, sender, binding override), so it deliberately does not
// require an address: a binding carries only overrides. The factory
// enforces role-specific completeness (session, receiver, and sender
// addresses) at build time.
func (c Config) Validate() error {
	if c.Session.Address != "" {
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
// password populates Session.Username/Password (when empty) and TLS
// material populates Session.TLS PEM fields.
func (c *Config) ApplyCredentials(set *connectivity.CredentialSet) error {
	if c == nil {
		return errors.New("amqp10: nil config")
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
		applyAMQP10TLSMaterial(&c.Session.TLS, set.TLS())
	}
	c.CredentialsURIRef = ""
	return nil
}
