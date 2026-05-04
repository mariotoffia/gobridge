package sqs

import (
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

// ReceiverConfig configures an SQS Receiver.
//
// SQS native DLQ maxReceiveCount should be set to at least
// (bridge max retries + 3) to avoid the SQS DLQ swallowing messages
// that the bridge would otherwise handle.
type ReceiverConfig struct {
	// QueueURL is the fully qualified SQS queue URL. Either QueueURL or
	// QueueName must be set.
	QueueURL string

	// QueueName is the logical queue name, resolved to a URL on startup.
	QueueName string

	// Region is the AWS region. Empty uses the SDK default chain.
	Region string

	// Endpoint overrides the SQS endpoint (for LocalStack / testing).
	Endpoint string

	// Profile selects an AWS shared-config profile.
	Profile string

	// MaxMessages is the maximum number of messages per ReceiveMessage
	// call (1-10). Default 10.
	MaxMessages int32

	// WaitTimeSeconds is the long-poll duration (0-20). Default 20.
	WaitTimeSeconds int32

	// VisibilityTimeout in seconds. Default 30.
	VisibilityTimeout int32

	// AutoExtend starts a background goroutine that renews visibility
	// at 50 % of VisibilityTimeout. Default true.
	AutoExtend *bool

	// InitTimeout is the timeout for the initialisation phase (client
	// creation and queue-URL resolution). Default 30s.
	InitTimeout time.Duration

	// PollBackoffInitial is the starting delay after a failed
	// ReceiveMessage call. Default 1s.
	PollBackoffInitial time.Duration

	// PollBackoffMax is the maximum delay between retries. Default 30s.
	PollBackoffMax time.Duration

	// PollBackoffMultiplier is the exponential growth factor for the
	// backoff delay. Default 2.0.
	PollBackoffMultiplier float64

	// SNSUnwrap, when true, detects SNS notification wrappers and
	// extracts the inner message body. Default false.
	SNSUnwrap bool

	// Client allows injecting a pre-built SQS client (for tests).
	// When set, Region/Endpoint/Profile are ignored.
	Client sqsAPI

	// Logger is an optional structured logger for trace/debug output.
	Logger *slog.Logger

	// Metrics is an optional metrics exporter for adapter-internal metrics.
	Metrics ports.MetricsExporter

	// Clock drives the delivery auto-extend ticker. When nil defaults
	// to clock.System (wall clock). Tests may inject a clocktest.Fake
	// to control tick firing deterministically.
	Clock clock.Clock
}

// SenderConfig configures an SQS Sender.
type SenderConfig struct {
	// QueueURL is the fully qualified SQS queue URL. Either QueueURL or
	// QueueName must be set.
	QueueURL string

	// QueueName is the logical queue name, resolved to a URL on startup.
	QueueName string

	// Region is the AWS region. Empty uses the SDK default chain.
	Region string

	// Endpoint overrides the SQS endpoint (for LocalStack / testing).
	Endpoint string

	// Profile selects an AWS shared-config profile.
	Profile string

	// DelaySeconds is the default delay before the message becomes
	// visible (0-900). Default 0.
	DelaySeconds int32

	// BatchSize is the number of entries per SendMessageBatch call
	// (1-10). Default 10.
	BatchSize int

	// Timeout is the per-call timeout for send operations. Default 30 s.
	Timeout time.Duration

	// MessageGroupID is the default FIFO message group. When non-empty,
	// FIFO behaviour is activated.
	MessageGroupID string

	// FIFO explicitly marks the queue as FIFO even when MessageGroupID
	// is empty (the group can still come from envelope headers).
	FIFO bool

	// Client allows injecting a pre-built SQS client (for tests).
	// When set, Region/Endpoint/Profile are ignored.
	Client sqsAPI

	// Logger is an optional structured logger for trace/debug output.
	Logger *slog.Logger

	// Metrics is an optional metrics exporter for adapter-internal metrics.
	Metrics ports.MetricsExporter

	// Clock provides message timestamps and operation timing.
	// When nil defaults to clock.System.
	Clock clock.Clock
}

func (c *ReceiverConfig) validate() error {
	if c.QueueURL == "" && c.QueueName == "" {
		return errors.New("sqs: either QueueURL or QueueName is required")
	}
	return nil
}

func (c *ReceiverConfig) applyDefaults() {
	if c.MaxMessages <= 0 || c.MaxMessages > 10 {
		c.MaxMessages = 10
	}
	if c.WaitTimeSeconds <= 0 {
		c.WaitTimeSeconds = 20
	} else if c.WaitTimeSeconds > 20 {
		c.WaitTimeSeconds = 20
	}
	if c.VisibilityTimeout <= 0 {
		c.VisibilityTimeout = 30
	}
	if c.AutoExtend == nil {
		t := true
		c.AutoExtend = &t
	}
	if c.InitTimeout <= 0 {
		c.InitTimeout = 30 * time.Second
	}
	if c.PollBackoffInitial <= 0 {
		c.PollBackoffInitial = time.Second
	}
	if c.PollBackoffMax <= 0 {
		c.PollBackoffMax = 30 * time.Second
	}
	if c.PollBackoffMultiplier <= 0 {
		c.PollBackoffMultiplier = 2.0
	}
	if c.Clock == nil {
		c.Clock = clock.System
	}
}

func (c *ReceiverConfig) autoExtendEnabled() bool {
	return c.AutoExtend == nil || *c.AutoExtend
}

func (c *SenderConfig) validate() error {
	if c.QueueURL == "" && c.QueueName == "" {
		return errors.New("sqs: either QueueURL or QueueName is required")
	}
	return nil
}

func (c *SenderConfig) applyDefaults() {
	if c.Clock == nil {
		c.Clock = clock.System
	}
	if c.DelaySeconds < 0 {
		c.DelaySeconds = 0
	} else if c.DelaySeconds > 900 {
		c.DelaySeconds = 900
	}
	if c.BatchSize <= 0 || c.BatchSize > 10 {
		c.BatchSize = 10
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
}

func (c *SenderConfig) isFIFO() bool {
	return c.FIFO || c.MessageGroupID != ""
}

// ReceiverConfigFromOptions extracts a ReceiverConfig from a generic
// options map as carried by ports.ReceiverSpec.Options.
func ReceiverConfigFromOptions(opts map[string]any) ReceiverConfig {
	cfg := ReceiverConfig{}
	cfg.QueueURL, _ = optString(opts, "queue_url")
	cfg.QueueName, _ = optString(opts, "queue_name")
	cfg.Region, _ = optString(opts, "region")
	cfg.Endpoint, _ = optString(opts, "endpoint")
	cfg.Profile, _ = optString(opts, "profile")
	cfg.MaxMessages = optInt32(opts, "max_messages", 10)
	cfg.WaitTimeSeconds = optInt32(opts, "wait_time_seconds", 20)
	cfg.VisibilityTimeout = optInt32(opts, "visibility_timeout", 30)
	if v, ok := optBool(opts, "auto_extend"); ok {
		cfg.AutoExtend = &v
	}
	cfg.SNSUnwrap, _ = optBool(opts, "sns_unwrap")
	return cfg
}

// SenderConfigFromOptions extracts a SenderConfig from a generic
// options map as carried by ports.SenderSpec.Options.
func SenderConfigFromOptions(opts map[string]any) SenderConfig {
	cfg := SenderConfig{}
	cfg.QueueURL, _ = optString(opts, "queue_url")
	cfg.QueueName, _ = optString(opts, "queue_name")
	cfg.Region, _ = optString(opts, "region")
	cfg.Endpoint, _ = optString(opts, "endpoint")
	cfg.Profile, _ = optString(opts, "profile")
	cfg.DelaySeconds = optInt32(opts, "delay_seconds", 0)
	cfg.BatchSize = int(optInt32(opts, "batch_size", 10))
	if v, ok := optDuration(opts, "timeout"); ok && v > 0 {
		cfg.Timeout = v
	}
	cfg.MessageGroupID, _ = optString(opts, "message_group_id")
	cfg.FIFO, _ = optBool(opts, "fifo")
	return cfg
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

func optDuration(m map[string]any, key string) (time.Duration, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch d := v.(type) {
	case time.Duration:
		if d < 0 {
			return 0, false
		}
		return d, true
	case string:
		parsed, err := time.ParseDuration(d)
		if err != nil || parsed < 0 {
			return 0, false
		}
		return parsed, true
	case int:
		if d < 0 {
			return 0, false
		}
		return time.Duration(d) * time.Second, true
	case int64:
		if d < 0 {
			return 0, false
		}
		return time.Duration(d) * time.Second, true
	case float64:
		if d < 0 || math.IsNaN(d) || math.IsInf(d, 0) {
			return 0, false
		}
		return time.Duration(d * float64(time.Second)), true
	default:
		return 0, false
	}
}
