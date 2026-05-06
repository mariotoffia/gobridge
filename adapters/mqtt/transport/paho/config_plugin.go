package paho

import (
	"errors"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contract.
var _ ports.CredentialedConfig = (*Config)(nil)

// Config is the typed PluginConfig for the MQTT (Eclipse Paho)
// transport. It nests session/sender role configs and is shared
// across SessionSpec.Config / ReceiverSpec.Config / SenderSpec.Config.
type Config struct {
	Session SessionOptions `mapstructure:"session" yaml:"session" json:"session"`
	Sender  SenderOptions  `mapstructure:"sender" yaml:"sender" json:"sender"`

	// CredentialsURIRef is the optional URI consulted by the bridge's
	// credential store at build time. Resolved material is applied
	// via ApplyCredentials.
	CredentialsURIRef string `mapstructure:"credentials_uri" yaml:"credentials_uri,omitempty" json:"credentials_uri,omitempty"`
}

// Kind reports the registry discriminator.
func (Config) Kind() string { return "mqtt.paho" }

// Validate validates the unified config. Empty role-specific fields
// are allowed because the same Config is reused across all three
// specs and not all roles are populated for each spec.
func (c Config) Validate() error {
	if c.Session.ClientID == "" && len(c.Session.BrokerURLs) == 0 && c.Sender.DefaultTopic == "" {
		return errors.New("mqtt: at least one of session.client_id, session.broker_urls, or sender.default_topic must be set")
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
		return errors.New("mqtt: nil config")
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
		applyTLSMaterial(&c.Session.TLS, set.TLS)
	}
	c.CredentialsURIRef = ""
	return nil
}
