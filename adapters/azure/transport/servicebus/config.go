package servicebus

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
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

	// SessionID locks the receiver to a specific ASB session. Cannot
	// be combined with SubQueue: the SDK's SessionReceiverOptions has
	// no sub-queue selector (azservicebus v1.10.0).
	SessionID string

	// UseSessions consumes a session-enabled entity WITHOUT pinning a
	// session id: the receiver accepts the next available session
	// (AcceptNextSessionForQueue / ...ForSubscription), drains it, and
	// rotates to the next session when a poll comes back empty with no
	// deliveries in flight — the SDK's round-robin pattern. Cannot be
	// combined with SessionID (which pins ONE session) or SubQueue
	// (the SDK's SessionReceiverOptions has no sub-queue selector).
	UseSessions bool

	// MaxMessages is the maximum number of messages per
	// ReceiveMessages call. Default 10, capped at 100. Forced to 1 in
	// ReceiveAndDelete mode: the broker deletes at receive time, so
	// every batched-but-not-yet-emitted message would be lost on
	// shutdown — a window that scales with MaxMessages.
	MaxMessages int

	// MaxWaitTime bounds a single ReceiveMessages long-poll. The Azure
	// SDK has no "max wait for the first message" option, so it is applied
	// as a per-receive context deadline: when it elapses with no messages
	// the poll loop simply re-issues. Default 30 s.
	MaxWaitTime time.Duration

	// ReceiveMode is "PeekLock" (default) or "ReceiveAndDelete"
	// (case-insensitive). Any other value is rejected by validate.
	ReceiveMode string

	// AllowAtMostOnce is the explicit opt-in that MUST be set to run
	// ReceiveMode "ReceiveAndDelete". That mode is AT-MOST-ONCE: the
	// broker deletes the message at receive time, so a crash — or a
	// malformed-envelope drop — after receive but before the downstream
	// send is UNRECOVERABLE loss, Ack is a no-op, and Retry is
	// unsupported. validate() rejects ReceiveAndDelete unless this is
	// true, and NewReceiver emits a loud startup warning, so the lossy
	// semantics can never be selected by accident. Ignored for PeekLock.
	AllowAtMostOnce bool

	// SubQueue selects a sub-queue: "", "deadletter", or
	// "transferdeadletter" (case-insensitive). Any other value is
	// rejected by validate so a typo can never silently consume the
	// main queue during a DLQ redrive.
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

	// MinAutoExtendInterval is the floor for the computed
	// auto-extend renewal interval. Default 1s.
	MinAutoExtendInterval time.Duration

	// MaxLockRenewalDuration caps the total wall-clock time a single
	// delivery's lock is auto-renewed. When the cap is reached the
	// delivery's processing context is cancelled and renewal stops, so
	// a hung pipeline cannot hold a message locked (invisible, never
	// redelivered, never DLQ'd) forever. Occurrences are counted by
	// MetricASBLockRenewalCapExceeded. Default 5m; <= 0 selects the
	// default.
	MaxLockRenewalDuration time.Duration

	// Connection holds Azure Service Bus connection/credential settings.
	Connection ConnectionConfig

	// Client allows injecting a pre-built receiver (for tests).
	// When set, Connection is ignored.
	Client asbAPI

	// Logger is an optional structured logger for trace/debug output.
	Logger *slog.Logger

	// Metrics is an optional metrics exporter for adapter-internal metrics.
	Metrics ports.MetricsExporter

	// Clock drives the delivery lock-renewal (auto-extend) ticker.
	// When nil defaults to clock.System (wall clock). Tests may inject
	// a clocktest.Fake to control tick firing deterministically.
	Clock clock.Clock
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

	// Timeout is the per-call timeout for send operations. SendBatch
	// applies it per chunk, not across the whole batch.
	// Default 30 s.
	Timeout time.Duration

	// Connection holds Azure Service Bus connection/credential settings.
	Connection ConnectionConfig

	// Client allows injecting a pre-built sender (for tests).
	// When set, Connection is ignored.
	Client asbSenderAPI

	// Logger is an optional structured logger for trace/debug output.
	Logger *slog.Logger

	// Metrics is an optional metrics exporter for adapter-internal metrics.
	Metrics ports.MetricsExporter

	// Clock provides message timestamps and operation timing.
	// When nil defaults to clock.System.
	Clock clock.Clock
}

// validateReceiveMode enforces the closed receive_mode value set,
// case-insensitively: "" (defaults to PeekLock), "PeekLock", or
// "ReceiveAndDelete". Shared by ReceiverConfig.validate (programmatic
// path) and Config.Validate (YAML plugin path).
func validateReceiveMode(mode string) error {
	switch {
	case mode == "",
		strings.EqualFold(mode, "PeekLock"),
		strings.EqualFold(mode, "ReceiveAndDelete"):
		return nil
	default:
		return fmt.Errorf("servicebus: receive_mode %q is invalid; must be \"PeekLock\" or \"ReceiveAndDelete\"", mode)
	}
}

// validateSubQueue enforces the closed sub_queue value set,
// case-insensitively: "", "deadletter", or "transferdeadletter". A typo
// (e.g. "dead-letter") must fail fast — silently receiving from the
// MAIN queue during a DLQ redrive re-processes live traffic.
func validateSubQueue(subQueue string) error {
	switch {
	case subQueue == "",
		strings.EqualFold(subQueue, "deadletter"),
		strings.EqualFold(subQueue, "transferdeadletter"):
		return nil
	default:
		return fmt.Errorf("servicebus: sub_queue %q is invalid; must be \"deadletter\" or \"transferdeadletter\"", subQueue)
	}
}

// validateReceiverEntityExclusive rejects a receiver config that names
// BOTH a queue and a topic/subscription. entityNameFor selects the
// queue whenever QueueName is set and silently ignores TopicName /
// SubscriptionName, so a both-set config would consume from the wrong
// entity with no startup error. Exactly one entity kind is allowed: a
// queue, OR a topic + subscription (HIGH-3).
func validateReceiverEntityExclusive(queueName, topicName, subscriptionName string) error {
	if queueName != "" && (topicName != "" || subscriptionName != "") {
		return errors.New("servicebus: receiver sets both queue_name and topic_name/subscription_name; exactly one entity kind is allowed (a queue OR a topic+subscription) — the queue would be selected and the topic silently ignored, so remove one")
	}
	return nil
}

// validateSenderEntityExclusive rejects a sender config that names BOTH
// a queue and a topic. entityName selects the queue whenever QueueName
// is set and silently ignores TopicName, so a both-set config would
// publish to the wrong entity with no startup error. Exactly one is
// allowed (HIGH-3).
func validateSenderEntityExclusive(queueName, topicName string) error {
	if queueName != "" && topicName != "" {
		return errors.New("servicebus: sender sets both queue_name and topic_name; exactly one is allowed — the queue would be selected and the topic silently ignored, so remove one")
	}
	return nil
}

func (c *ReceiverConfig) validate() error {
	if c.QueueName == "" && (c.TopicName == "" || c.SubscriptionName == "") {
		return errors.New("servicebus: either QueueName or TopicName+SubscriptionName is required")
	}
	if err := validateReceiverEntityExclusive(c.QueueName, c.TopicName, c.SubscriptionName); err != nil {
		return err
	}
	if c.Client == nil && c.Connection.ConnectionString.IsZero() && c.Connection.Namespace == "" {
		return errors.New("servicebus: Connection.ConnectionString or Connection.Namespace is required")
	}
	if err := validateReceiveMode(c.ReceiveMode); err != nil {
		return err
	}
	if c.receiveAndDelete() && !c.AllowAtMostOnce {
		// ReceiveAndDelete is AT-MOST-ONCE: the broker deletes at receive,
		// Ack is a no-op and Retry is unsupported, so any crash after
		// receive is unrecoverable loss. Refuse to start it unless the
		// operator has explicitly accepted those semantics.
		return errors.New("servicebus: receive_mode ReceiveAndDelete is at-most-once (the broker deletes at receive time; a crash after receive is unrecoverable message loss, Ack is a no-op, Retry is unsupported) and must be explicitly enabled with allow_at_most_once=true")
	}
	if err := validateSubQueue(c.SubQueue); err != nil {
		return err
	}
	if c.SessionID != "" && c.SubQueue != "" {
		// azservicebus v1.10.0 SessionReceiverOptions has no SubQueue
		// selector: a session receiver cannot target a sub-queue, and
		// silently ignoring sub_queue would consume the wrong entity.
		return errors.New("servicebus: session_id cannot be combined with sub_queue (not supported by the Azure SDK)")
	}
	if c.UseSessions && c.SessionID != "" {
		return errors.New("servicebus: use_sessions cannot be combined with session_id (session_id already pins the receiver to one session)")
	}
	if c.UseSessions && c.SubQueue != "" {
		return errors.New("servicebus: use_sessions cannot be combined with sub_queue (not supported by the Azure SDK)")
	}
	return nil
}

func (c *ReceiverConfig) applyDefaults() {
	if c.MaxMessages <= 0 {
		c.MaxMessages = 10
	} else if c.MaxMessages > 100 {
		c.MaxMessages = 100
	}
	if c.receiveAndDelete() {
		// ReceiveAndDelete pre-settles at the broker: every received-but-
		// not-yet-emitted message is unrecoverable on shutdown or emit
		// error. Cap the loss window at one message. NewReceiver warns
		// when this clamps a larger configured value.
		c.MaxMessages = 1
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
	if c.MinAutoExtendInterval <= 0 {
		c.MinAutoExtendInterval = time.Second
	}
	if c.MaxLockRenewalDuration <= 0 {
		c.MaxLockRenewalDuration = defaultMaxLockRenewalDuration
	}
	if c.Clock == nil {
		c.Clock = clock.System
	}
}

// defaultMaxLockRenewalDuration bounds per-delivery lock auto-renewal
// when max_lock_renewal_duration is unset (finding: a hung pipeline
// must not hold a message invisible forever).
const defaultMaxLockRenewalDuration = 5 * time.Minute

func (c *ReceiverConfig) autoExtendEnabled() bool {
	return c.AutoExtend == nil || *c.AutoExtend
}

// receiveAndDelete reports whether the receiver runs in
// ReceiveModeReceiveAndDelete, where the broker removes the message at
// receive time. In that mode there is no lock to renew or settle:
// auto-extend is disabled and Ack/Extend are no-ops while Retry reports
// ErrNotSupported (see asbDelivery). Matching is case-insensitive;
// validate rejects every other spelling.
func (c *ReceiverConfig) receiveAndDelete() bool {
	return strings.EqualFold(c.ReceiveMode, "ReceiveAndDelete")
}

func (c *SenderConfig) validate() error {
	if c.QueueName == "" && c.TopicName == "" {
		return errors.New("servicebus: either QueueName or TopicName is required")
	}
	if err := validateSenderEntityExclusive(c.QueueName, c.TopicName); err != nil {
		return err
	}
	if c.Client == nil && c.Connection.ConnectionString.IsZero() && c.Connection.Namespace == "" {
		return errors.New("servicebus: Connection.ConnectionString or Connection.Namespace is required")
	}
	return nil
}

func (c *SenderConfig) applyDefaults() {
	if c.Clock == nil {
		c.Clock = clock.System
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 10
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
}
