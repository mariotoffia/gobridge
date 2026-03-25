package servicebus

import (
	"errors"
	"time"
)

// ReceiverConfig configures an Azure Service Bus Receiver.
type ReceiverConfig struct {
	// QueueName is the Service Bus queue to receive from.
	// Either QueueName or (TopicName + SubscriptionName) must be set.
	QueueName string

	// TopicName is the Service Bus topic to receive from.
	TopicName string

	// SubscriptionName is the subscription on TopicName.
	SubscriptionName string

	// SessionID locks the receiver to a specific ASB session.
	SessionID string

	// MaxMessages is the maximum number of messages per
	// ReceiveMessages call. Default 10, capped at 100.
	MaxMessages int

	// MaxWaitTime is the maximum time to wait for messages.
	// Default 30 s.
	MaxWaitTime time.Duration

	// Prefetch sets the prefetch count on the AMQP link. Default 0
	// (SDK default).
	Prefetch int32

	// ReceiveMode is "PeekLock" (default) or "ReceiveAndDelete".
	ReceiveMode string

	// SubQueue selects a sub-queue: "", "deadletter", or
	// "transferdeadletter".
	SubQueue string

	// LockDuration is the expected message lock duration configured
	// on the queue/subscription. Used to compute the auto-extend
	// renewal interval (50 % of this value). Default 30 s.
	// When a received message has a LockedUntil timestamp, the actual
	// remaining lock time takes precedence over this value.
	LockDuration time.Duration

	// AutoExtend starts a background goroutine that renews the
	// message lock at 50 % of the lock duration. Default true.
	AutoExtend *bool

	// Connection holds Azure Service Bus connection/credential settings.
	Connection ConnectionConfig

	// Client allows injecting a pre-built receiver (for tests).
	// When set, Connection is ignored.
	Client asbAPI
}

// SenderConfig configures an Azure Service Bus Sender.
type SenderConfig struct {
	// QueueName is the Service Bus queue to send to.
	// Either QueueName or TopicName must be set.
	QueueName string

	// TopicName is the Service Bus topic to send to.
	TopicName string

	// DefaultSessionID is the default ASB session id for sent
	// messages.
	DefaultSessionID string

	// BatchSize is a hint for the number of messages per batch.
	// ASB batches are size-limited; this is an upper bound on count.
	// Default 10.
	BatchSize int

	// Timeout is the per-call timeout for send operations.
	// Default 30 s.
	Timeout time.Duration

	// Connection holds Azure Service Bus connection/credential settings.
	Connection ConnectionConfig

	// Client allows injecting a pre-built sender (for tests).
	// When set, Connection is ignored.
	Client asbSenderAPI
}

func (c *ReceiverConfig) validate() error {
	if c.QueueName == "" && (c.TopicName == "" || c.SubscriptionName == "") {
		return errors.New("servicebus: either QueueName or TopicName+SubscriptionName is required")
	}
	if c.Client == nil && c.Connection.ConnectionString == "" && c.Connection.Namespace == "" {
		return errors.New("servicebus: Connection.ConnectionString or Connection.Namespace is required")
	}
	return nil
}

func (c *ReceiverConfig) applyDefaults() {
	if c.MaxMessages <= 0 {
		c.MaxMessages = 10
	} else if c.MaxMessages > 100 {
		c.MaxMessages = 100
	}
	if c.MaxWaitTime <= 0 {
		c.MaxWaitTime = 30 * time.Second
	}
	if c.LockDuration <= 0 {
		c.LockDuration = 30 * time.Second
	}
	if c.AutoExtend == nil {
		t := true
		c.AutoExtend = &t
	}
}

func (c *ReceiverConfig) autoExtendEnabled() bool {
	return c.AutoExtend == nil || *c.AutoExtend
}

func (c *SenderConfig) validate() error {
	if c.QueueName == "" && c.TopicName == "" {
		return errors.New("servicebus: either QueueName or TopicName is required")
	}
	if c.Client == nil && c.Connection.ConnectionString == "" && c.Connection.Namespace == "" {
		return errors.New("servicebus: Connection.ConnectionString or Connection.Namespace is required")
	}
	return nil
}

func (c *SenderConfig) applyDefaults() {
	if c.BatchSize <= 0 {
		c.BatchSize = 10
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
}

// ReceiverConfigFromOptions extracts a ReceiverConfig from a generic
// options map as carried by ports.ReceiverSpec.Options.
func ReceiverConfigFromOptions(opts map[string]any) ReceiverConfig {
	cfg := ReceiverConfig{}
	cfg.QueueName, _ = optString(opts, "queue_name")
	cfg.TopicName, _ = optString(opts, "topic_name")
	cfg.SubscriptionName, _ = optString(opts, "subscription_name")
	cfg.SessionID, _ = optString(opts, "session_id")
	cfg.MaxMessages = optInt(opts, "max_messages", 10)
	if v, ok := opts["max_wait_time"].(time.Duration); ok && v > 0 {
		cfg.MaxWaitTime = v
	}
	cfg.Prefetch = optInt32(opts, "prefetch", 0)
	cfg.ReceiveMode, _ = optString(opts, "receive_mode")
	cfg.SubQueue, _ = optString(opts, "sub_queue")
	if v, ok := opts["lock_duration"].(time.Duration); ok && v > 0 {
		cfg.LockDuration = v
	}
	if v, ok := optBool(opts, "auto_extend"); ok {
		cfg.AutoExtend = &v
	}
	cfg.Connection = connectionFromOptions(opts)
	return cfg
}

// SenderConfigFromOptions extracts a SenderConfig from a generic
// options map as carried by ports.SenderSpec.Options.
func SenderConfigFromOptions(opts map[string]any) SenderConfig {
	cfg := SenderConfig{}
	cfg.QueueName, _ = optString(opts, "queue_name")
	cfg.TopicName, _ = optString(opts, "topic_name")
	cfg.DefaultSessionID, _ = optString(opts, "default_session_id")
	cfg.BatchSize = optInt(opts, "batch_size", 10)
	if v, ok := opts["timeout"].(time.Duration); ok && v > 0 {
		cfg.Timeout = v
	}
	cfg.Connection = connectionFromOptions(opts)
	return cfg
}

func connectionFromOptions(opts map[string]any) ConnectionConfig {
	var c ConnectionConfig
	c.ConnectionString, _ = optString(opts, "connection_string")
	c.Namespace, _ = optString(opts, "namespace")
	c.UseManagedIdentity, _ = optBool(opts, "use_managed_identity")
	c.TenantID, _ = optString(opts, "tenant_id")
	c.ClientID, _ = optString(opts, "client_id")
	c.ClientSecret, _ = optString(opts, "client_secret")
	c.CaPEM, _ = optString(opts, "ca_pem")
	c.InsecureSkipVerify, _ = optBool(opts, "insecure_skip_verify")
	return c
}

func optString(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func optBool(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func optInt(m map[string]any, key string, fallback int) int {
	v, ok := m[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return fallback
	}
}

func optInt32(m map[string]any, key string, fallback int32) int32 {
	v, ok := m[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return int32(n)
	case int32:
		return n
	case int64:
		return int32(n)
	case float64:
		return int32(n)
	default:
		return fallback
	}
}
