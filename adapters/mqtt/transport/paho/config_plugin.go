package paho

import (
	"errors"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contract.
var (
	_ ports.CredentialedConfig                = (*Config)(nil)
	_ ports.FreezableConfig                   = Config{}
	_ ports.FreezableConfig                   = (*Config)(nil)
	_ ports.DurableSessionIdentityConfig      = Config{}
	_ ports.DurableSessionIdentityConfig      = (*Config)(nil)
	_ ports.ReplicaIdentityConfig             = Config{}
	_ ports.ReplicaIdentityConfig             = (*Config)(nil)
	_ ports.PostAcquireActivationTimingConfig = Config{}
	_ ports.PostAcquireActivationTimingConfig = (*Config)(nil)
)

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

	// clientIDSuffixIdentity is process-shared by default and may be overridden
	// by tests. Keeping the pointer in Config makes value copies resolve the same
	// effective suffix as the config instance they were copied from.
	clientIDSuffixIdentity *clientIDSuffixProcessIdentity `mapstructure:"-" yaml:"-" json:"-"`
}

// FreezePluginConfig implements ports.FreezableConfig. Mutable configuration
// collections and nested config pointers become deep-owned. Clock and
// clientIDSuffixIdentity are intentionally shared opaque runtime dependencies:
// clock identity/injection must be preserved, and suffix preflight/build must
// resolve through the same process-stable state.
func (c Config) FreezePluginConfig() ports.PluginConfig {
	frozen := c
	frozen.Session.BrokerURLs = append([]string(nil), c.Session.BrokerURLs...)
	if c.Session.TLS != nil {
		tlsCopy := *c.Session.TLS
		frozen.Session.TLS = &tlsCopy
	}
	if c.Session.Will != nil {
		willCopy := *c.Session.Will
		frozen.Session.Will = &willCopy
	}
	return &frozen
}

// PostAcquireActivationTiming exposes one conservative hard bound for every
// sequential phase a durable managed-filter activation may execute. Two broker
// connections cover initial Start plus cleanup recycle. Four reconcile-owned
// waits cover initial SUBSCRIBE, exact UNSUBSCRIBE, bounded ingress quiescence,
// and replacement-generation SUBSCRIBE. Two replay windows cover crash residue
// followed by filters removed in the current attempt. The SDK reconnect-attempt
// timeout is nested inside each connection await and is therefore not added
// again.
func (c Config) PostAcquireActivationTiming(mode connectivity.SessionMode) ports.SessionActivationTiming {
	if mode != connectivity.SessionPersistent && mode != connectivity.SessionExclusive {
		return ports.SessionActivationTiming{}
	}
	connectTimeout := c.Session.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = DefaultConnectTimeout
	}
	reconcileTimeout := c.Session.ReconcileTimeout
	if reconcileTimeout <= 0 {
		reconcileTimeout = DefaultReconcileTimeout
	}
	replayGrace := c.Session.UnmatchedGrace
	if replayGrace <= 0 {
		replayGrace = DefaultUnmatchedGrace
	}
	return ports.SessionActivationTiming{WorstCaseDuration: conservativeActivationDuration(
		connectTimeout, connectTimeout,
		reconcileTimeout, reconcileTimeout, reconcileTimeout, reconcileTimeout,
		replayGrace, replayGrace,
	)}
}

// conservativeActivationDuration saturates instead of allowing a maliciously
// large duration to overflow negative and bypass the composition-root bound.
func conservativeActivationDuration(phases ...time.Duration) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)
	var total time.Duration
	for _, phase := range phases {
		if phase <= 0 {
			continue
		}
		if total > maxDuration-phase {
			return maxDuration
		}
		total += phase
	}
	return total
}

// Kind reports the registry discriminator.
func (Config) Kind() string { return "mqtt.paho" }

// Validate validates the unified config. Empty role-specific fields
// are allowed because the same Config is reused across all three
// specs and not all roles are populated for each spec: a receiver or
// sender may inherit its connection identity from a session, so even
// a fully empty options block is valid at parse time. The factory
// enforces the real requirements (client_id, broker_urls) on the
// effective merged config at build time.
func (c Config) Validate() error {
	if c.Sender.QoS > 2 {
		return fmt.Errorf("mqtt: sender.qos must be 0, 1, or 2, got %d", c.Sender.QoS)
	}
	if err := c.Session.Will.Validate(); err != nil {
		return err
	}
	// HIGH-4: fail closed on cleartext username/password over a non-TLS
	// broker. Guarded internally by broker_urls being present, so a
	// receiver/sender spec that carries no session connection block (empty
	// broker_urls) is unaffected; the session spec — where broker_urls and
	// credentials live — is the one that trips the gate. When credentials are
	// still PENDING (credentials_uri set, username not yet resolved) the gate
	// naturally passes here because hasCredentials() is false, and
	// ApplyCredentials re-runs it post-resolution.
	if err := c.Session.validatePlaintextCredentials(); err != nil {
		return err
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
	if set.Password() != nil {
		if c.Session.Username == "" {
			c.Session.Username = set.Password().Username()
		}
		if c.Session.Password.IsZero() {
			c.Session.Password = set.Password().Password()
		}
	}
	if set.TLS() != nil {
		applyTLSMaterial(&c.Session.TLS, set.TLS())
	}
	c.CredentialsURIRef = ""
	// HIGH-4: the resolution above may have supplied the FIRST credentials
	// (a credentials_uri-only config that deferred the plaintext gate at
	// parse time). Re-run the gate now so resolved credentials over a non-TLS
	// broker still fail closed unless explicitly opted in.
	if err := c.Session.validatePlaintextCredentials(); err != nil {
		return err
	}
	return nil
}
