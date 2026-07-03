package sqs

import (
	"errors"
	"log/slog"
	"strings"
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
	// FIFO ordering safety (Finding 5): the route runner dispatches
	// deliveries concurrently, so a single ReceiveMessage returning
	// several messages of one MessageGroupId could let them be reordered.
	// SQS keeps a FIFO group locked to its in-flight message until that
	// message is deleted, so MaxMessages=1 guarantees at most one
	// in-flight message per group and preserves per-group order without
	// serialising in the shared runner. Detected from the configured URL
	// or name (.fifo suffix); Run re-checks the resolved URL as a safety
	// net for QueueName-only configs.
	if isFIFOQueue(c.QueueURL) || isFIFOQueue(c.QueueName) {
		c.MaxMessages = 1
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
	// FIFO fail-fast: a ".fifo" queue send without a MessageGroupId is a
	// deterministic config fault SQS rejects at runtime with
	// MissingParameter. Require a message-group configuration up front:
	// either a default MessageGroupID, or the explicit FIFO flag as the
	// operator's opt-in to per-envelope groups via the
	// x-bridge.ordering-key header (a missing header is then rejected
	// per-message before the SDK call — see Sender.validateFIFOGroup).
	if (isFIFOQueue(c.QueueURL) || isFIFOQueue(c.QueueName)) && !c.FIFO && c.MessageGroupID == "" {
		return errors.New(
			"sqs: FIFO queue (\".fifo\" suffix) requires message_group_id " +
				"(default message group) or fifo: true (per-envelope group " +
				"via the x-bridge.ordering-key header)")
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

// isFIFO reports whether the sender must apply FIFO send semantics
// (MessageGroupId / MessageDeduplicationId). True when the operator set
// the FIFO flag or a default MessageGroupID, and ALSO auto-detected
// from the ".fifo" suffix of the configured queue URL/name so a FIFO
// queue can never be sent to with the standard-queue shape (the broker
// would reject every such send with MissingParameter).
func (c *SenderConfig) isFIFO() bool {
	return c.FIFO || c.MessageGroupID != "" ||
		isFIFOQueue(c.QueueURL) || isFIFOQueue(c.QueueName)
}

// isFIFOQueue reports whether an SQS queue URL or name denotes a FIFO
// queue. AWS requires FIFO queue names (and therefore the trailing path
// segment of their URLs) to end in ".fifo".
func isFIFOQueue(urlOrName string) bool {
	return strings.HasSuffix(urlOrName, ".fifo")
}
