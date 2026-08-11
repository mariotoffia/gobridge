package paho

import (
	"errors"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
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
	_ ports.TransportFailoverTimingConfig     = Config{}
	_ ports.TransportFailoverTimingConfig     = (*Config)(nil)
	_ ports.IngressMemoryConfig               = Config{}
	_ ports.IngressMemoryConfig               = (*Config)(nil)
	_ ports.IngressMemoryProfileConfig        = (*Config)(nil)
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

// TransportFailoverTiming reuses the complete post-acquire activation phase
// calculator. It includes initial connect, managed cleanup and replay,
// recycle/reconnect, final subscription convergence, and grace windows once.
func (c Config) TransportFailoverTiming(mode connectivity.SessionMode) ports.TransportFailoverTiming {
	activation := c.PostAcquireActivationTiming(mode)
	return ports.TransportFailoverTiming{PostTakeoverActivation: activation.WorstCaseDuration}
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
func (Config) Kind() string { return QualifiedKind }

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
	// fail closed on cleartext username/password over a non-TLS
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
	if !c.Session.receiveMaximumExplicit && c.Session.ReceiveMaximum == 0 {
		if err := c.validateIngressMemoryPrerequisites(); err != nil {
			return err
		}
	} else {
		if err := c.ValidateIngressMemory(0); err != nil {
			return err
		}
	}
	return nil
}

// ValidateEffectiveSession performs the resource-free validation required once
// a Config is attached to a concrete SessionMode. It validates the same broker
// domain and stable connection identity that NewSession consumes, but it never
// constructs a client or dials a broker. Deployment composition roots may call
// it to fail at synth/startup preflight without duplicating Paho rules.
func (c Config) ValidateEffectiveSession(mode connectivity.SessionMode) error {
	if mode == "" {
		mode = connectivity.SessionEphemeral
	}
	c.Session.normalizeBrokerURLs()
	if err := c.ValidateSessionMode(mode); err != nil {
		return err
	}
	if c.Session.ClientID == "" {
		return errors.New("mqtt: client_id is required")
	}
	if suffix := c.Session.ClientIDSuffix; suffix != "" {
		if suffix != ClientIDSuffixHostname && suffix != ClientIDSuffixNonce {
			return fmt.Errorf("mqtt: unsupported client_id_suffix %q", suffix)
		}
		if suffix == ClientIDSuffixNonce && mode != connectivity.SessionEphemeral {
			return errors.New("mqtt: client_id_suffix nonce is valid only for session_mode: ephemeral")
		}
		if mode == connectivity.SessionExclusive {
			return errors.New("mqtt: client_id_suffix must not be set for session_mode: exclusive")
		}
	}
	if len(c.Session.BrokerURLs) == 0 {
		return errors.New("mqtt: at least one broker URL is required")
	}
	if err := c.Session.validatePlaintextCredentials(); err != nil {
		return err
	}
	return c.Session.Will.Validate()
}

// validateIngressMemoryPrerequisites checks the packet ceiling and proves the
// configured budget can fit one legal receive slot, one dispatch slot, and the
// raw/decode crossing slot. It deliberately does not choose a Receive Maximum:
// deployment profiles may derive a safe value after parse, while generic bridge
// preflight later calls ValidateIngressMemory and applies the default 192.
func (c Config) validateIngressMemoryPrerequisites() error {
	session := c.Session.normalizedIngressMemory()
	minimum, err := IngressMemoryBound(session.MaxPayloadBytes, 1, 0)
	if err != nil {
		return err
	}
	if minimum > session.IngressMemoryBudgetBytes {
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"mqtt: ingress_memory_budget_bytes %d cannot fit one legal receive/dispatch window and the raw predecode crossing (requires at least %d bytes)",
			session.IngressMemoryBudgetBytes,
			minimum,
		))
	}
	return nil
}

// ValidateIngressMemory implements ports.IngressMemoryConfig. It checks the
// complete effective receive, dispatch, route, and current-packet window against
// the configured per-session byte budget.
func (c Config) ValidateIngressMemory(routeMaxInFlight uint64) error {
	session := c.Session.normalizedIngressMemory()
	bound, err := IngressMemoryBound(
		session.MaxPayloadBytes,
		session.ReceiveMaximum,
		routeMaxInFlight,
	)
	if err != nil {
		return err
	}
	if bound > session.IngressMemoryBudgetBytes {
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"mqtt: ingress memory bound %d bytes exceeds ingress_memory_budget_bytes %d "+
				"(max_payload_bytes=%d receive_maximum=%d dispatch_capacity=%d route_max_in_flight=%d)",
			bound,
			session.IngressMemoryBudgetBytes,
			session.MaxPayloadBytes,
			session.ReceiveMaximum,
			session.ReceiveMaximum,
			routeMaxInFlight,
		))
	}
	return nil
}

// ConfigureIngressMemory implements ports.IngressMemoryProfileConfig. Defaults
// are derived to the largest safe Receive Maximum. Explicit receive/budget
// values are preserved when safe and rejected when they exceed the assigned
// deployment budget.
func (c *Config) ConfigureIngressMemory(budgetBytes, routeMaxInFlight uint64) error {
	if c == nil {
		return shared.ErrInvalidConfig.WithMessage("mqtt: cannot configure ingress memory on nil config")
	}
	session := c.Session.normalizedIngressMemory()
	effectiveBudget := budgetBytes
	if session.ingressMemoryBudgetExplicit {
		if session.IngressMemoryBudgetBytes > budgetBytes {
			return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
				"mqtt: explicit ingress_memory_budget_bytes %d exceeds deployment allocation %d",
				session.IngressMemoryBudgetBytes, budgetBytes,
			))
		}
		effectiveBudget = session.IngressMemoryBudgetBytes
	}
	if effectiveBudget == 0 {
		return shared.ErrInvalidConfig.WithMessage("mqtt: deployment ingress memory allocation must be greater than zero")
	}
	session.IngressMemoryBudgetBytes = effectiveBudget

	if session.receiveMaximumExplicit {
		candidate := *c
		candidate.Session = session
		if err := candidate.ValidateIngressMemory(routeMaxInFlight); err != nil {
			return err
		}
		c.Session = session
		return nil
	}

	receiveMaximum, err := LargestSafeReceiveMaximum(
		session.MaxPayloadBytes,
		effectiveBudget,
		routeMaxInFlight,
	)
	if err != nil {
		return err
	}
	session.ReceiveMaximum = receiveMaximum
	c.Session = session
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
	// the resolution above may have supplied the FIRST credentials
	// (a credentials_uri-only config that deferred the plaintext gate at
	// parse time). Re-run the gate now so resolved credentials over a non-TLS
	// broker still fail closed unless explicitly opted in.
	if err := c.Session.validatePlaintextCredentials(); err != nil {
		return err
	}
	return nil
}
