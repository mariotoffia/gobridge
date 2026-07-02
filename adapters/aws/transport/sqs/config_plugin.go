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
}

// CredentialsURI implements ports.CredentialedConfig.
func (c *Config) CredentialsURI() string {
	if c == nil {
		return ""
	}
	return c.CredentialsURIRef
}

// ApplyCredentials implements ports.CredentialedConfig. SQS does not
// consume password material from the typed Config at build time —
// runtime rotation lives on Sender/Receiver via the CredentialAware
// path. Apply therefore only clears the URI to mark resolution as
// done so the URI is not re-resolved on subsequent passes.
//
// Pre-existing inline values would take precedence here too, but
// since SQS Config carries no inline auth fields the contract is
// degenerate.
func (c *Config) ApplyCredentials(_ *connectivity.CredentialSet) error {
	if c == nil {
		return errors.New("sqs: nil config")
	}
	c.CredentialsURIRef = ""
	return nil
}

// Kind reports the registry discriminator.
func (Config) Kind() string { return "aws.sqs" }

// Validate checks for required fields and consistency.
func (c Config) Validate() error {
	if c.QueueURL == "" && c.QueueName == "" {
		return errors.New("sqs: either queue_url or queue_name is required")
	}
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
		InitTimeout:           c.InitTimeout,
		PollBackoffInitial:    c.PollBackoffInitial,
		PollBackoffMax:        c.PollBackoffMax,
		PollBackoffMultiplier: c.PollBackoffMultiplier,
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
// ports.VisibilityTimeoutConfig (Finding 2 / D2).
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
		QueueURL:       c.QueueURL,
		QueueName:      c.QueueName,
		Region:         c.Region,
		Endpoint:       c.Endpoint,
		Profile:        c.Profile,
		DelaySeconds:   c.DelaySeconds,
		BatchSize:      c.BatchSize,
		Timeout:        c.Timeout,
		MessageGroupID: c.MessageGroupID,
		FIFO:           c.FIFO,
	}
}
