package servicebus

import (
	"errors"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time interface contract.
var _ ports.CredentialedConfig = (*Config)(nil)
var _ ports.VisibilityTimeoutConfig = (*Config)(nil)

// Config is the typed PluginConfig for the Azure Service Bus
// transport. Service Bus has no session, so only Receiver/Sender
// roles are exposed; the same Config is shared across
// ReceiverSpec.Config / SenderSpec.Config.
type Config struct {
	Receiver   ReceiverParams   `mapstructure:"receiver" yaml:"receiver" json:"receiver"`
	Sender     SenderParams     `mapstructure:"sender" yaml:"sender" json:"sender"`
	Connection ConnectionConfig `mapstructure:"connection" yaml:"connection" json:"connection"`

	// CredentialsURIRef is the optional URI consulted by the bridge's
	// credential store at build time. The resolved material is
	// applied to Connection via ApplyCredentials.
	CredentialsURIRef string `mapstructure:"credentials_uri" yaml:"credentials_uri,omitempty" json:"credentials_uri,omitempty"`
}

// ReceiverParams holds user-settable receiver fields.
type ReceiverParams struct {
	QueueName        string `mapstructure:"queue_name" yaml:"queue_name" json:"queue_name"`
	TopicName        string `mapstructure:"topic_name" yaml:"topic_name" json:"topic_name"`
	SubscriptionName string `mapstructure:"subscription_name" yaml:"subscription_name" json:"subscription_name"`
	SessionID        string `mapstructure:"session_id" yaml:"session_id" json:"session_id"`
	// UseSessions consumes a session-enabled entity by accepting the
	// next available session and rotating between sessions; see
	// ReceiverConfig.UseSessions. Mutually exclusive with session_id
	// and sub_queue.
	UseSessions  bool          `mapstructure:"use_sessions" yaml:"use_sessions" json:"use_sessions"`
	MaxMessages  int           `mapstructure:"max_messages" yaml:"max_messages" json:"max_messages"`
	MaxWaitTime  time.Duration `mapstructure:"max_wait_time" yaml:"max_wait_time" json:"max_wait_time"`
	ReceiveMode  string        `mapstructure:"receive_mode" yaml:"receive_mode" json:"receive_mode"`
	SubQueue     string        `mapstructure:"sub_queue" yaml:"sub_queue" json:"sub_queue"`
	LockDuration time.Duration `mapstructure:"lock_duration" yaml:"lock_duration" json:"lock_duration"`
	AutoExtend   *bool         `mapstructure:"auto_extend" yaml:"auto_extend" json:"auto_extend"`
	// MaxLockRenewalDuration caps total per-delivery lock auto-renewal
	// wall time (default 5m); see ReceiverConfig.MaxLockRenewalDuration.
	MaxLockRenewalDuration time.Duration `mapstructure:"max_lock_renewal_duration" yaml:"max_lock_renewal_duration" json:"max_lock_renewal_duration"`
}

// SenderParams holds user-settable sender fields.
type SenderParams struct {
	QueueName        string        `mapstructure:"queue_name" yaml:"queue_name" json:"queue_name"`
	TopicName        string        `mapstructure:"topic_name" yaml:"topic_name" json:"topic_name"`
	DefaultSessionID string        `mapstructure:"default_session_id" yaml:"default_session_id" json:"default_session_id"`
	BatchSize        int           `mapstructure:"batch_size" yaml:"batch_size" json:"batch_size"`
	Timeout          time.Duration `mapstructure:"timeout" yaml:"timeout" json:"timeout"`
}

// Kind reports the registry discriminator.
func (Config) Kind() string { return "azure.servicebus" }

// Validate checks field ranges and internal consistency. It runs at
// parse time on EVERY attachment point that reuses this Config shape
// (receiver, sender, binding override), so it deliberately does not
// require an entity name: a binding carries only overrides. The
// factory enforces role-specific completeness (ValidateReceiverEntity
// / ValidateSenderEntity) at build time.
func (c Config) Validate() error {
	// Service Bus accepts a message lock duration of 5s..5min; 0 means
	// "use the 30s default" (see EffectiveVisibilityTimeout). A value
	// outside the broker range is rejected at entity/receiver setup, so
	// fail fast here naming the field and the allowed range.
	if c.Receiver.LockDuration != 0 &&
		(c.Receiver.LockDuration < 5*time.Second || c.Receiver.LockDuration > 5*time.Minute) {
		return errors.New("servicebus: receiver.lock_duration must be in [5s,5m]")
	}
	// A sub-second max_wait_time turns the long-poll into a hot loop
	// (an expired per-receive deadline is a normal empty poll, re-issued
	// immediately). It is also the symptom of a bare-int YAML duration
	// decoding as nanoseconds — reject with a hint. 0 selects the 30s
	// default.
	if c.Receiver.MaxWaitTime != 0 && c.Receiver.MaxWaitTime < time.Second {
		return fmt.Errorf("servicebus: receiver.max_wait_time %v is below the 1s floor (a hot receive loop); use a duration string like \"30s\"", c.Receiver.MaxWaitTime)
	}
	if err := validateReceiveMode(c.Receiver.ReceiveMode); err != nil {
		return err
	}
	if err := validateSubQueue(c.Receiver.SubQueue); err != nil {
		return err
	}
	if c.Receiver.SessionID != "" && c.Receiver.SubQueue != "" {
		return errors.New("servicebus: receiver.session_id cannot be combined with receiver.sub_queue (not supported by the Azure SDK)")
	}
	if c.Receiver.UseSessions && c.Receiver.SessionID != "" {
		return errors.New("servicebus: receiver.use_sessions cannot be combined with receiver.session_id (session_id already pins the receiver to one session)")
	}
	if c.Receiver.UseSessions && c.Receiver.SubQueue != "" {
		return errors.New("servicebus: receiver.use_sessions cannot be combined with receiver.sub_queue (not supported by the Azure SDK)")
	}
	return nil
}

// ValidateReceiverEntity enforces that the config names a receive
// entity: a queue, or a topic + subscription pair. Called where a
// concrete Receiver is built (factory) — not from Validate, which
// also runs on binding overrides that legitimately omit the entity.
func (c Config) ValidateReceiverEntity() error {
	if c.Receiver.QueueName == "" && (c.Receiver.TopicName == "" || c.Receiver.SubscriptionName == "") {
		return errors.New("servicebus: receiver requires queue_name, or topic_name with subscription_name")
	}
	return nil
}

// ValidateSenderEntity enforces that the config names a send entity:
// a queue or a topic. Same build-boundary rationale as
// ValidateReceiverEntity.
func (c Config) ValidateSenderEntity() error {
	if c.Sender.QueueName == "" && c.Sender.TopicName == "" {
		return errors.New("servicebus: sender requires queue_name or topic_name")
	}
	return nil
}

// EffectiveVisibilityTimeout returns the receiver lock duration this
// config resolves to at runtime — the Service Bus analog of a visibility
// window — honouring the same 30s default ReceiverConfig.applyDefaults
// applies when lock_duration is unset. It satisfies
// ports.VisibilityTimeoutConfig so the builder threads this per-route
// value into the runtime validator instead of the Factory's 30s constant,
// correctly guarding a route whose lock_duration is shorter than the
// default against a SendTimeout that exceeds half the lock window
// (Finding 2). It mirrors the identical SQS EffectiveVisibilityTimeout().
func (c Config) EffectiveVisibilityTimeout() time.Duration {
	if c.Receiver.LockDuration > 0 {
		return c.Receiver.LockDuration
	}
	return 30 * time.Second
}

// AutoExtendEnabled reports whether the receiver renews the message lock
// in the background while a message is in flight, mirroring
// ReceiverConfig.autoExtendEnabled (default on when unset). It satisfies
// ports.VisibilityTimeoutConfig so the validator can skip the finite
// SendTimeout-vs-window check for auto-extended routes (Finding 2 / D2).
func (c Config) AutoExtendEnabled() bool {
	return c.Receiver.AutoExtend == nil || *c.Receiver.AutoExtend
}

func (c Config) toReceiverConfig() ReceiverConfig {
	return ReceiverConfig{
		QueueName:              c.Receiver.QueueName,
		TopicName:              c.Receiver.TopicName,
		SubscriptionName:       c.Receiver.SubscriptionName,
		SessionID:              c.Receiver.SessionID,
		UseSessions:            c.Receiver.UseSessions,
		MaxMessages:            c.Receiver.MaxMessages,
		MaxWaitTime:            c.Receiver.MaxWaitTime,
		ReceiveMode:            c.Receiver.ReceiveMode,
		SubQueue:               c.Receiver.SubQueue,
		LockDuration:           c.Receiver.LockDuration,
		AutoExtend:             c.Receiver.AutoExtend,
		MaxLockRenewalDuration: c.Receiver.MaxLockRenewalDuration,
		Connection:             c.Connection,
	}
}

func (c Config) toSenderConfig() SenderConfig {
	return SenderConfig{
		QueueName:        c.Sender.QueueName,
		TopicName:        c.Sender.TopicName,
		DefaultSessionID: c.Sender.DefaultSessionID,
		BatchSize:        c.Sender.BatchSize,
		Timeout:          c.Sender.Timeout,
		Connection:       c.Connection,
	}
}

// CredentialsURI implements ports.CredentialedConfig.
func (c *Config) CredentialsURI() string {
	if c == nil {
		return ""
	}
	return c.CredentialsURIRef
}

// ApplyCredentials implements ports.CredentialedConfig. The resolved
// credential set is merged into Connection via the existing
// credentialsToConnection helper, which honours the SAS connection
// string vs. AAD client-secret split. Pre-existing inline values on
// Connection take precedence: credentialsToConnection only writes
// when the resolved material differs.
func (c *Config) ApplyCredentials(set *connectivity.CredentialSet) error {
	if c == nil {
		return errors.New("servicebus: nil config")
	}
	if set == nil {
		c.CredentialsURIRef = ""
		return nil
	}
	merged, _ := credentialsToConnection(c.Connection, set)
	c.Connection = merged
	c.CredentialsURIRef = ""
	return nil
}
