package sqs

import (
	"errors"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contract: only *Config satisfies
// CredentialedConfig because ApplyCredentials mutates the receiver.
var _ ports.CredentialedConfig = (*Config)(nil)
var _ ports.FreezableConfig = Config{}
var _ ports.VisibilityTimeoutConfig = (*Config)(nil)

// Config is the typed PluginConfig for the SQS transport. A single
// struct holds session/receiver/sender fields; the factory derives
// internal ReceiverConfig / SenderConfig from it.
type Config struct {
	// Common
	QueueURL  string `mapstructure:"queue_url" yaml:"queue_url" json:"queue_url"`
	QueueName string `mapstructure:"queue_name" yaml:"queue_name" json:"queue_name"`
	Region    string `mapstructure:"region" yaml:"region" json:"region"`
	Endpoint  string `mapstructure:"endpoint" yaml:"endpoint" json:"endpoint"`
	Profile   string `mapstructure:"profile" yaml:"profile" json:"profile"`

	// CredentialsURIRef is the optional URI consulted by the bridge's
	// credential store at build time. The resolved material is
	// applied via ApplyCredentials below; runtime rotation is handled
	// separately by Sender/Receiver.ApplyCredentials.
	CredentialsURIRef string `mapstructure:"credentials_uri" yaml:"credentials_uri,omitempty" json:"credentials_uri,omitempty"`

	// Receiver
	MaxMessages       int32 `mapstructure:"max_messages" yaml:"max_messages" json:"max_messages"`
	WaitTimeSeconds   int32 `mapstructure:"wait_time_seconds" yaml:"wait_time_seconds" json:"wait_time_seconds"`
	VisibilityTimeout int32 `mapstructure:"visibility_timeout" yaml:"visibility_timeout" json:"visibility_timeout"`
	AutoExtend        *bool `mapstructure:"auto_extend" yaml:"auto_extend" json:"auto_extend"`
	SNSUnwrap         bool  `mapstructure:"sns_unwrap" yaml:"sns_unwrap" json:"sns_unwrap"`

	// PoisonMaxReceives surfaces the receiver's adapter-enforced poison
	// backstop to config-driven (YAML) deployments. A malformed message the
	// receiver cannot convert is normally dropped WITHOUT a delete so the
	// source queue's native redrive policy (maxReceiveCount -> DLQ) can move
	// it; a queue with NO redrive policy would redeliver it forever. When this
	// is > 0 and the message's ApproximateReceiveCount reaches it, the receiver
	// deletes the message to break the hot loop. 0/omitted disables the
	// backstop (native redrive only). Set it ABOVE the queue's native
	// maxReceiveCount so native redrive still wins where configured.
	PoisonMaxReceives int32 `mapstructure:"poison_max_receives" yaml:"poison_max_receives,omitempty" json:"poison_max_receives,omitempty"`

	// PoisonDropWithoutDLQ is the explicit opt-in required for the single most
	// destructive poison backstop setting (poison_max_receives == 1, which
	// deletes a poison message on its first conversion failure). Without it a
	// value of 1 is rejected at config time. See ReceiverConfig for details.
	PoisonDropWithoutDLQ bool `mapstructure:"poison_drop_without_dlq" yaml:"poison_drop_without_dlq,omitempty" json:"poison_drop_without_dlq,omitempty"`

	// Receiver resilience tuning. These map directly to the
	// ReceiverConfig backoff/init knobs that previously had no plugin
	// surface, so outage/failover behaviour could not be tuned from
	// deployment config (Finding 10). Durations decode from strings such
	// as "30s" via the config parser's StringToTimeDuration hook. Zero /
	// omitted values fall back to the ReceiverConfig defaults.
	InitTimeout           time.Duration `mapstructure:"init_timeout" yaml:"init_timeout,omitempty" json:"init_timeout,omitempty"`
	PollBackoffInitial    time.Duration `mapstructure:"poll_backoff_initial" yaml:"poll_backoff_initial,omitempty" json:"poll_backoff_initial,omitempty"`
	PollBackoffMax        time.Duration `mapstructure:"poll_backoff_max" yaml:"poll_backoff_max,omitempty" json:"poll_backoff_max,omitempty"`
	PollBackoffMultiplier float64       `mapstructure:"poll_backoff_multiplier" yaml:"poll_backoff_multiplier,omitempty" json:"poll_backoff_multiplier,omitempty"`

	// Sender
	DelaySeconds   int32         `mapstructure:"delay_seconds" yaml:"delay_seconds" json:"delay_seconds"`
	BatchSize      int           `mapstructure:"batch_size" yaml:"batch_size" json:"batch_size"`
	Timeout        time.Duration `mapstructure:"timeout" yaml:"timeout,omitempty" json:"timeout,omitempty"`
	MessageGroupID string        `mapstructure:"message_group_id" yaml:"message_group_id" json:"message_group_id"`
	FIFO           bool          `mapstructure:"fifo" yaml:"fifo" json:"fifo"`

	// MaxMessageBytes surfaces the sender's message-size ceiling (body +
	// attributes) to config-driven (YAML) deployments — mirroring the
	// PollBackoff* knobs that likewise had no plugin surface. 0/omitted keeps
	// the 262144 (256 KiB) default; raise it only to match a queue whose
	// MaximumMessageSize has been provisioned above 256 KiB. Without this an
	// operator who raises a queue's limit via YAML cannot lift the ceiling, so
	// a body over 256 KiB silently drops ALL egress attributes — including the
	// rank-0 x-bridge.idempotency-key / traceparent headers (Finding 4).
	MaxMessageBytes int `mapstructure:"max_message_bytes" yaml:"max_message_bytes,omitempty" json:"max_message_bytes,omitempty"`

	// resolvedCreds holds the static credential material resolved from
	// CredentialsURIRef at build time (ApplyCredentials). It is projected
	// into ReceiverConfig/SenderConfig.InitialCredentials so the INITIAL
	// SQS client is built with it — previously ApplyCredentials discarded
	// the material and the first client always fell back to the ambient
	// SDK chain (Finding 3/HIGH). It is unexported and never decoded from
	// config; the redaction-safe PasswordCredential keeps it log-safe.
	resolvedCreds *connectivity.PasswordCredential
}

// FreezePluginConfig returns a deep-owned build snapshot. resolvedCreds is a
// secret-safe immutable value object and may be shared until ApplyCredentials
// replaces the pointer on the frozen copy.
func (c Config) FreezePluginConfig() ports.PluginConfig {
	frozen := c
	if c.AutoExtend != nil {
		autoExtend := *c.AutoExtend
		frozen.AutoExtend = &autoExtend
	}
	return &frozen
}

// CredentialsURI implements ports.CredentialedConfig.
func (c *Config) CredentialsURI() string {
	if c == nil {
		return ""
	}
	return c.CredentialsURIRef
}

// ApplyCredentials implements ports.CredentialedConfig. The bridge
// resolves CredentialsURIRef through the credential store and hands the
// material here BEFORE the factory builds the Receiver/Sender. The
// resolved password credential is retained (resolvedCreds) and threaded
// into the initial SQS client via toReceiverConfig/toSenderConfig →
// ensureClient, so a `credentials_uri` actually changes the identity of
// the first client instead of being silently dropped in favour of the
// ambient SDK chain (Finding 3). The URI is then cleared to mark
// resolution done so it is not re-resolved on subsequent passes.
//
// Temporary/STS material (ASIA-prefixed access key) is rejected up front
// (fail the build) rather than producing a client that would fail every
// request — see ErrTemporaryCredentialsUnsupported (Finding 6).
func (c *Config) ApplyCredentials(set *connectivity.CredentialSet) error {
	if c == nil {
		return errors.New("sqs: nil config")
	}
	if set != nil && set.Password() != nil {
		pw := set.Password()
		if isTemporaryAccessKeyID(pw.Username()) {
			return ErrTemporaryCredentialsUnsupported
		}
		c.resolvedCreds = pw
	}
	c.CredentialsURIRef = ""
	return nil
}

// Kind reports the registry discriminator.
func (Config) Kind() string { return QualifiedKind }

// DefaultConfig returns a Config pre-filled with the documented receiver
// defaults that are otherwise indistinguishable from an explicit zero on
// the typed YAML path: wait_time_seconds (20, long-poll) and max_messages
// (10). The registry decoder (register.go) decodes into this value so an
// OMITTED key keeps the default while an EXPLICIT `wait_time_seconds: 0`
// (or `max_messages: 0`) survives decode as 0 and is rejected with a
// clear error instead of being silently coerced back to the default
// (Finding 12). Mirrors paho.DefaultConfig().
func DefaultConfig() Config {
	return Config{
		MaxMessages:     10,
		WaitTimeSeconds: 20,
	}
}

// Validate checks field ranges and internal consistency. It runs at
// parse time on EVERY attachment point that reuses this Config shape
// (receiver, sender, binding override), so it deliberately does not
// require a queue reference: a binding carries only overrides and a
// receiver/sender may leave the queue to its own spec. Completeness
// (queue_url or queue_name) is enforced by ValidateQueue at the
// points that actually build a Receiver/Sender.
func (c Config) Validate() error {
	if c.MaxMessages < 0 || c.MaxMessages > 10 {
		return errors.New("sqs: max_messages must be in [1,10]")
	}
	if c.WaitTimeSeconds < 0 || c.WaitTimeSeconds > 20 {
		return errors.New("sqs: wait_time_seconds must be in [0,20]")
	}
	// SQS caps the visibility timeout at 12h (43200s); 0 means "use the
	// 30s default" (see EffectiveVisibilityTimeout). A value above the
	// broker limit would be rejected at receive time, so fail fast here.
	if c.VisibilityTimeout < 0 || c.VisibilityTimeout > 43200 {
		return errors.New("sqs: visibility_timeout must be in [0,43200] seconds (0-12h)")
	}
	if c.BatchSize < 0 || c.BatchSize > 10 {
		return errors.New("sqs: batch_size must be in [1,10]")
	}
	if c.DelaySeconds < 0 || c.DelaySeconds > 900 {
		return errors.New("sqs: delay_seconds must be in [0,900]")
	}
	if c.PollBackoffMultiplier != 0 && c.PollBackoffMultiplier < 1 {
		return errors.New("sqs: poll_backoff_multiplier must be >= 1.0")
	}
	if c.InitTimeout < 0 || c.PollBackoffInitial < 0 || c.PollBackoffMax < 0 {
		return errors.New("sqs: init_timeout and poll_backoff durations must not be negative")
	}
	if c.PollBackoffInitial > 0 && c.PollBackoffMax > 0 && c.PollBackoffMax < c.PollBackoffInitial {
		return errors.New("sqs: poll_backoff_max must be >= poll_backoff_initial")
	}
	// poison_max_receives is an adapter-enforced backstop for poison messages
	// (Chunk 13): 0 disables it (rely on native redrive), any positive
	// value bounds the redelivery hot loop. Its destructive delete must not
	// preempt a native DLQ, so poison_max_receives == 1 (drop on first receive)
	// requires the explicit poison_drop_without_dlq opt-in; the queue-aware
	// "must exceed native maxReceiveCount" guard runs at startup where the live
	// redrive policy is readable (checkRedrivePolicy).
	if err := validatePoisonBackstop(c.PoisonMaxReceives, c.PoisonDropWithoutDLQ); err != nil {
		return err
	}
	return nil
}

// ValidateQueue enforces that the config carries a queue reference.
// Called where a concrete Receiver/Sender is built from this Config
// (factory, CDK bridgecfg builder) — not from Validate, which also
// runs on binding overrides that legitimately omit the queue.
func (c Config) ValidateQueue() error {
	if c.QueueURL == "" && c.QueueName == "" {
		return errors.New("sqs: either queue_url or queue_name is required")
	}
	return nil
}

// toReceiverConfig projects the unified Config onto the internal
// ReceiverConfig used by the SQS receiver.
func (c Config) toReceiverConfig() ReceiverConfig {
	return ReceiverConfig{
		QueueURL:              c.QueueURL,
		QueueName:             c.QueueName,
		Region:                c.Region,
		Endpoint:              c.Endpoint,
		Profile:               c.Profile,
		MaxMessages:           c.MaxMessages,
		WaitTimeSeconds:       c.WaitTimeSeconds,
		VisibilityTimeout:     c.VisibilityTimeout,
		AutoExtend:            c.AutoExtend,
		SNSUnwrap:             c.SNSUnwrap,
		PoisonMaxReceives:     c.PoisonMaxReceives,
		PoisonDropWithoutDLQ:  c.PoisonDropWithoutDLQ,
		InitTimeout:           c.InitTimeout,
		PollBackoffInitial:    c.PollBackoffInitial,
		PollBackoffMax:        c.PollBackoffMax,
		PollBackoffMultiplier: c.PollBackoffMultiplier,
		InitialCredentials:    c.resolvedCreds,
	}
}

// EffectiveVisibilityTimeout returns the receiver visibility timeout
// this config resolves to at runtime, honouring the same 30s default the
// receiver applies when visibility_timeout is unset. It satisfies
// ports.VisibilityTimeoutConfig, so the builder threads this per-route
// value into the runtime validator's SourceVisibilityTimeout in
// preference to the hardcoded Factory.VisibilityTimeout() constant
// (Finding 2, wired in bridge/builder_complete.go, Phase 1b).
func (c Config) EffectiveVisibilityTimeout() time.Duration {
	if c.VisibilityTimeout > 0 {
		return time.Duration(c.VisibilityTimeout) * time.Second
	}
	return 30 * time.Second
}

// AutoExtendEnabled reports whether the receiver actually renews message
// visibility in the background while a message is in flight. This is the
// *effective* signal the validator needs, not the bare config flag: the
// SQS runtime starts the renewal goroutine only when auto-extend is on
// AND the effective window meets the runtime floor
// (minAutoExtendVisibilitySeconds — see newDelivery in acl_delivery.go).
// Below the floor the runtime runs a fixed, non-renewed window, so the
// validator must still enforce the finite SendTimeout-vs-window check to
// prevent source redelivery mid-send. It satisfies
// ports.VisibilityTimeoutConfig (Finding 2 /).
func (c Config) AutoExtendEnabled() bool {
	flag := c.AutoExtend == nil || *c.AutoExtend
	effSecs := c.VisibilityTimeout
	if effSecs <= 0 {
		effSecs = 30 // mirror applyDefaults: an unset window defaults to 30s
	}
	return flag && effSecs >= minAutoExtendVisibilitySeconds
}

// toSenderConfig projects the unified Config onto the internal
// SenderConfig used by the SQS sender.
func (c Config) toSenderConfig() SenderConfig {
	return SenderConfig{
		QueueURL:           c.QueueURL,
		QueueName:          c.QueueName,
		Region:             c.Region,
		Endpoint:           c.Endpoint,
		Profile:            c.Profile,
		DelaySeconds:       c.DelaySeconds,
		BatchSize:          c.BatchSize,
		Timeout:            c.Timeout,
		MessageGroupID:     c.MessageGroupID,
		FIFO:               c.FIFO,
		MaxMessageBytes:    c.MaxMessageBytes,
		InitialCredentials: c.resolvedCreds,
	}
}
