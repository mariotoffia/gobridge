package sqs

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
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

	// PoisonMaxReceives is an adapter-enforced backstop for malformed
	// ("poison") messages the receiver cannot convert to an Envelope. A
	// poison message is normally dropped WITHOUT a DeleteMessage so the source
	// queue's native redrive policy (maxReceiveCount -> DLQ) can move it;
	// a source queue with NO redrive policy would instead redeliver it every
	// visibility timeout forever (a permanent hot loop). When
	// PoisonMaxReceives > 0 and a poison message's ApproximateReceiveCount
	// reaches it, the receiver DELETES the message — a controlled, observable
	// drop (SQSPoisonDropped counter + Error log) — to break that loop. 0
	// (default) disables the backstop and relies entirely on native redrive.
	// Set it comfortably ABOVE the source queue's native maxReceiveCount so
	// native redrive still owns the message on correctly-configured queues; a
	// message only climbs this high when nothing is draining it.
	//
	// ponytail: a controlled adapter-side delete is the ceiling — a true
	// bridge-DLQ handoff for conversion failures needs a ports.Delivery-level
	// poison sink the receiver does not own, because the message never becomes
	// a Delivery (conversion fails first).
	PoisonMaxReceives int32

	// PoisonDropWithoutDLQ is the explicit opt-in required for the single most
	// destructive poison backstop setting: a PoisonMaxReceives of exactly 1,
	// which DELETES a poison message on its FIRST conversion failure — no
	// redelivery, no window for a native DLQ to preserve the payload. Without
	// this flag PoisonMaxReceives == 1 is rejected at config time because a
	// single failed receive destroying data is almost never intended. It does
	// NOT relax the startup guard (checkRedrivePolicy) that refuses a backstop
	// which would preempt an EXISTING native redrive policy — that pre-emption
	// is always a data-loss fault regardless of this flag, because a DLQ that
	// exists would have preserved the payload.
	PoisonDropWithoutDLQ bool

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

	// InitialCredentials, when set, builds the initial SQS client with a
	// static credentials provider instead of the ambient SDK chain. It
	// carries the material resolved from a plugin `credentials_uri` at
	// build time (Config.ApplyCredentials → toReceiverConfig). Temporary
	// (STS) material is rejected when the client is built.
	InitialCredentials *connectivity.PasswordCredential
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

	// MaxMessageBytes overrides the SQS message-size ceiling (body plus
	// attributes) the sender enforces when selecting egress attributes.
	// Zero keeps the 262144 (256 KiB) default; raise it only to match a
	// queue whose MaximumMessageSize has been provisioned above 256 KiB,
	// otherwise a large body silently drops ALL attributes — including the
	// rank-0 idempotency-key / traceparent headers. The factory
	// projects it from the plugin Config and applies it via
	// WithMaxMessageBytes.
	MaxMessageBytes int

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

	// InitialCredentials, when set, builds the initial SQS client with a
	// static credentials provider instead of the ambient SDK chain. It
	// carries the material resolved from a plugin `credentials_uri` at
	// build time (Config.ApplyCredentials → toSenderConfig). Temporary
	// (STS) material is rejected when the client is built.
	InitialCredentials *connectivity.PasswordCredential
}

func (c *ReceiverConfig) validate() error {
	if c.QueueURL == "" && c.QueueName == "" {
		return errors.New("sqs: either QueueURL or QueueName is required")
	}
	if err := validatePoisonBackstop(c.PoisonMaxReceives, c.PoisonDropWithoutDLQ); err != nil {
		return err
	}
	return nil
}

// validatePoisonBackstop enforces the static, queue-INDEPENDENT invariants of
// the adapter poison backstop, shared by the plugin Config.Validate and the
// internal ReceiverConfig.validate so every construction path agrees:
//
//   - PoisonMaxReceives must be >= 0 (0 disables the backstop).
//   - PoisonMaxReceives == 1 is refused unless PoisonDropWithoutDLQ is set: a
//     value of 1 DELETES a poison message on its first conversion failure with
//     no redelivery and no DLQ copy — almost never intended, so it demands an
//     explicit opt-in.
//
// The queue-AWARE guard — refusing a backstop that would preempt an EXISTING
// native redrive policy (PoisonMaxReceives <= native maxReceiveCount) — needs
// the live RedrivePolicy and therefore runs at startup in checkRedrivePolicy,
// not here.
func validatePoisonBackstop(maxReceives int32, dropWithoutDLQ bool) error {
	if maxReceives < 0 {
		return errors.New("sqs: poison_max_receives must be >= 0 (0 disables the adapter poison backstop)")
	}
	if maxReceives == 1 && !dropWithoutDLQ {
		return errors.New("sqs: poison_max_receives == 1 destroys a poison message on its first " +
			"conversion failure (no redelivery, no DLQ copy); use poison_max_receives >= 2, or set " +
			"poison_drop_without_dlq: true to explicitly accept destructive single-receive drops")
	}
	return nil
}

func (c *ReceiverConfig) applyDefaults() {
	if c.MaxMessages <= 0 || c.MaxMessages > 10 {
		c.MaxMessages = 10
	}
	// FIFO ordering safety: the route runner dispatches
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
	// FIFO + per-message DelaySeconds fail-fast: AWS rejects a non-zero
	// DelaySeconds on a FIFO SendMessage/SendMessageBatch entry, so every
	// send would DLQ at runtime as ErrInvalidPayload. Reject the
	// combination at build instead, mirroring the FIFO
	// message-group cross-validation above. FIFO is detected from the
	// explicit flag, a default group, or the ".fifo" suffix.
	if c.DelaySeconds > 0 && c.isFIFO() {
		return errors.New(
			"sqs: delay_seconds is not supported on FIFO queues; AWS rejects " +
				"per-message DelaySeconds on a FIFO send (use a per-queue delay " +
				"or a standard queue)")
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
