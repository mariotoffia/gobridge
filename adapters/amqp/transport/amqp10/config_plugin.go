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
}

// SenderParams holds user-settable sender fields.
type SenderParams struct {
	Address        string        `mapstructure:"address" yaml:"address" json:"address"`
	Timeout        time.Duration `mapstructure:"timeout" yaml:"timeout" json:"timeout"`
	DurabilityMode uint32        `mapstructure:"durability_mode" yaml:"durability_mode" json:"durability_mode"`
	Routing        RoutingType   `mapstructure:"routing" yaml:"routing" json:"routing"`
}

// Kind reports the plugin discriminator.
func (Config) Kind() string { return "amqp.amqp10" }

// Validate checks required fields per role. Empty role-specific
// fields are allowed because the same Config is reused across all
// three specs and not all roles are populated for each spec.
func (c Config) Validate() error {
	if c.Session.Address == "" && c.Receiver.Address == "" && c.Sender.Address == "" {
		return errors.New("amqp10: at least one of session.address, receiver.address, or sender.address is required")
	}
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
	if set.Password != nil {
		if c.Session.Username == "" {
			c.Session.Username = set.Password.Username
		}
		if c.Session.Password == "" {
			c.Session.Password = set.Password.Password
		}
	}
	if set.TLS != nil {
		applyAMQP10TLSMaterial(&c.Session.TLS, set.TLS)
	}
	c.CredentialsURIRef = ""
	return nil
}
