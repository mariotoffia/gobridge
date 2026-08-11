package paho

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Sender implements ports.Sender for MQTT publishing.
//
// For QoS 1, Send blocks until PUBACK is received from the broker.
// For QoS 2, Send blocks until PUBCOMP is received.
// This means a nil return from Send confirms broker acceptance.
type Sender struct {
	session *Session
	opts    SenderOptions
	logger  *slog.Logger
	metrics ports.MetricsExporter
}

var _ ports.Sender = (*Sender)(nil)
var _ ports.NonDurableEgressReporter = (*Sender)(nil)

// NewSender creates a Sender bound to the given Session.
func NewSender(session *Session, opts SenderOptions) *Sender {
	m := session.metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	return &Sender{session: session, opts: opts, logger: session.logger, metrics: m}
}

// Send publishes the envelope to the MQTT broker.
//
// The publish topic is derived from msg.Address; when Address is empty,
// SenderOptions.DefaultTopic is used as a fallback. msg.Envelope.Subject()
// is the LOGICAL event subject and is propagated to the broker as the
// HeaderGobridgeSubject user property — it never selects the publish
// topic. When neither msg.Address nor opts.DefaultTopic is set, Send
// returns shared.ErrInvalidTopic without contacting the broker.
//
// Headers are mapped to MQTT 5 user properties. Message expiry is
// derived from the session clock.
//
// Returns nil when the broker has accepted the message (PUBACK / PUBCOMP).
// Returns a classified shared.BridgeError on failure.
func (s *Sender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	if env == nil {
		return shared.ErrInvalidPayload.WithMessage("nil envelope")
	}

	conn := s.session.connection()
	if conn == nil {
		return shared.ErrUnavailable.WithMessage("MQTT session not connected")
	}

	topic := msg.Address
	if topic == "" {
		topic = s.opts.DefaultTopic
	}
	if topic == "" {
		return shared.ErrInvalidTopic.WithMessage("no topic specified and no default topic configured")
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "mqtt: publishing",
			"topic", topic,
			"qos", s.opts.QoS,
			"payload_len", len(env.Payload()),
			"envelope_id", env.ID(),
		)
	}

	// Apply timeout: honour whichever of the configured sender timeout and
	// the caller's deadline is stricter (see applyTimeout for the rule).
	ctx, timeoutCancel := s.applyTimeout(ctx)
	defer timeoutCancel()

	sessionTag := shared.Tag{Key: shared.TagKeySessionID, Value: s.session.opts.ClientID}

	// Publish through the ACL's domain-shaped seam rather than the raw
	// autopaho.ConnectionManager: the seam serialises the envelope and returns
	// only the reason code, keeping port-side egress SDK-free.
	start := s.session.clock().Now()
	resp, err := conn.PublishEnvelope(ctx, env, topic, s.opts, s.session.clock())
	elapsed := s.session.clock().Since(start)

	if err != nil {
		s.metrics.Timer(MetricMQTTPublishLatency, elapsed, sessionTag)
		s.metrics.Counter(MetricMQTTPublishFailures, 1, sessionTag)
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "mqtt: publish failed",
				"topic", topic, "error", err)
		}
		return MapError(err)
	}

	if resp.ReasonCode != 0 {
		if berr := s.publishReasonError(resp.ReasonCode, topic); berr != nil {
			s.metrics.Timer(MetricMQTTPublishLatency, elapsed, sessionTag)
			s.metrics.Counter(MetricMQTTPublishFailures, 1, sessionTag)
			if logging.DebugEnabled(s.logger) {
				s.logger.Log(ctx, logging.LevelDebug, "mqtt: publish rejected",
					"topic", topic,
					"reason_code", fmt.Sprintf("0x%02X", resp.ReasonCode))
			}
			return berr
		}
	}

	s.metrics.Timer(MetricMQTTPublishLatency, elapsed, sessionTag)

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "mqtt: published",
			"topic", topic,
			"envelope_id", env.ID(),
			"duration", elapsed,
		)
	}

	return nil
}

// publishReasonError maps a non-zero PUBACK/PUBREC reason code to a
// classified BridgeError (nil for success codes 0x00 / 0x10), tagging it
// with the topic and reason code. It attaches a ThrottleRetryAfter hint
// ONLY for 0x97 (Quota exceeded) — the single PUBACK/PUBREC reason code
// that signals throttling. 0x93 (Receive Maximum exceeded) and 0xA1
// (Subscription Identifiers not supported) are NOT valid PUBACK/PUBREC
// reason codes, so they get no back-off hint (they classify as generic
// errors); the old dead checks for them were removed (finding 7).
func (s *Sender) publishReasonError(code byte, topic string) *shared.BridgeError {
	berr := MapPublishReasonCode(code)
	if berr == nil {
		return nil
	}
	result := berr.With("topic", topic).
		With("reason_code", fmt.Sprintf("0x%02X", code))
	if code == 0x97 {
		hint := s.opts.ThrottleRetryAfter
		if hint <= 0 {
			hint = 500 * time.Millisecond
		}
		result = result.WithRetryAfter(hint)
	}
	return result
}

// NonDurableEgress implements ports.NonDurableEgressReporter. It reports
// true for QoS 1/2 because autopaho keeps the outbound packet queue in
// memory: a publish in flight at process death is lost and QoS 2 is not
// exactly-once across a restart, even though the PUBACK/PUBCOMP handshake
// otherwise proves broker acceptance. QoS 0 is best-effort by protocol and
// makes no delivery claim, so it reports false. The bridge uses this to
// reason about egress durability per route (see the interface doc).
func (s *Sender) NonDurableEgress() bool {
	return s.opts.QoS >= 1
}

// defaultSendTimeout is the safety-net fallback used when
// SenderOptions.Timeout is zero and the context has no deadline.
const defaultSendTimeout = 60 * time.Second

// applyTimeout derives the send context's deadline from SenderOptions.Timeout
// and the caller's own deadline, honouring whichever is STRICTER:
//
//   - Timeout <= 0 (unset): apply the 60 s safety net ONLY when the caller
//     imposed no deadline (direct library consumers). A bridge route always
//     wraps the send in context.WithTimeout(policy.send_timeout), so the net
//     never shadows a route deadline.
//   - Timeout > 0 (configured): apply it whenever it is stricter than the
//     caller's remaining deadline (or there is none). This stops an
//     operator-set options.sender.timeout from being silently shadowed by a
//     looser route policy.send_timeout. A configured timeout LOOSER
//     than the caller's deadline is ignored — we never extend the caller's
//     ceiling, so the route send_timeout remains the upper bound.
func (s *Sender) applyTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, hasDeadline := ctx.Deadline()

	timeout := s.opts.Timeout
	if timeout <= 0 {
		if hasDeadline {
			return ctx, func() {}
		}
		return context.WithTimeout(ctx, defaultSendTimeout)
	}

	// Compare the caller's remaining budget against the configured timeout via
	// clock.System (never time.Until): a context deadline is wall-clock — set by
	// the route dispatcher's context.WithTimeout — and clock.System is that same
	// wall clock, so this matches how the context itself will expire. applyTimeout
	// is also exercised without a Session, so there is no injectable clock at this
	// seam.
	if hasDeadline && deadline.Sub(clock.System.Now()) <= timeout {
		return ctx, func() {} // caller's deadline is already stricter
	}
	return context.WithTimeout(ctx, timeout)
}
