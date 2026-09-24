package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// RouteConfig describes a route to be added to the Runtime.
type RouteConfig struct {
	ID     string
	Policy routing.RoutePolicy
	// Bindings carries the configured destinations for this route.
	// Each DestinationBinding.Address is the transport-level destination
	// (queue URL, topic name, exchange/routing-key, etc.) and is the
	// single source of truth for "where to send"; it is distinct from
	// messaging.Envelope.Subject, which is the logical message subject
	// and must never be overwritten with the binding Address.
	Bindings           []routing.DestinationBinding
	Resolver           ports.DestinationResolver
	Processors         []ports.Processor
	SourceCapabilities []ports.Capability

	// Senders maps binding IDs to their respective senders for
	// DirectHold content-based dispatch. When a resolver selects a
	// binding, the runner looks up the sender in this map. If the
	// binding ID is not found, it falls back to the route's default
	// sender. Optional; when nil all bindings use the default sender.
	Senders map[string]ports.Sender

	// AddressValidators maps binding IDs to the AddressValidator the
	// owning transport supplies (via TransportFactory.AddressValidator).
	// The runtime invokes the validator against every fully-rendered
	// destination address before dispatch and surfaces a non-nil
	// validation error as shared.ErrInvalidTopic. Bindings whose
	// transport returns a nil validator are simply omitted from this
	// map, in which case the runtime skips validation. Optional.
	AddressValidators map[string]ports.AddressValidator

	// SourceVisibilityTimeout is the visibility timeout of the source
	// transport (e.g. SQS VisibilityTimeout). When set, the validator
	// checks that SendTimeout does not exceed half this value to
	// prevent duplicate processing from source redelivery. Zero means
	// unknown or not applicable (validation is skipped).
	SourceVisibilityTimeout time.Duration

	// SourceAutoExtend reports whether the source transport renews the
	// visibility/lock window in the background while a message is in
	// flight (SQS/ASB auto_extend). When true the SourceVisibilityTimeout
	// check is skipped: the source will not redeliver during a long send
	// (barring repeated renewal failure), so a short window paired with
	// auto-extend is valid and must not be rejected.
	SourceAutoExtend bool

	// SourceSettlementRecoveryWait is how long the source's session waits for the
	// deliveries this route already accepted to settle before it recycles its
	// broker connection to recover stranded settlements (MQTT persistent and
	// exclusive sessions do). It is the OUTER bound of the WHOLE recovery
	// attempt, not a budget reserved for the settling: the same wait also covers
	// the session serialization gate, the teardown drain, and the disconnect,
	// reconnect and reconcile that follow. The validator checks
	// send_retry_budget + route.SendWedgeCeiling(send_timeout) — the send timeout
	// plus its wedge grace — against it, which is necessary but not
	// sufficient — a long processor chain or a dead-letter write on the same
	// held delivery still eats into what is left for the recycle. Zero means the
	// source never recycles for that reason, or the transport has no opinion,
	// and the check is skipped.
	SourceSettlementRecoveryWait time.Duration

	// SourceTransport is the identity of the transport feeding this route
	// (the RegisterTransportFactory name declared under `transport:`, or the
	// adapter's canonical PluginConfig.Kind). The runtime uses it to strip
	// foreign redelivery-count headers on ingress so an untrusted producer on a
	// count-less source cannot forge another transport's count key. Empty
	// disables the strip. Populated by the builder from the resolved receiver
	// transport; optional for programmatic callers.
	SourceTransport string

	// SourceRedeliveryRefusal is the source transport's own account of why this
	// route's source will NOT redeliver an unsettled message, and empty when it
	// will (or when the transport has no opinion). It exists so the direct_hold
	// refusal can name which precondition failed — a QoS 0 subscription and a
	// session the broker discards are the same verdict with different fixes, and
	// only the transport knows which one it is. Populated by the builder from
	// ports.SourceRedeliveryConfig; optional for programmatic callers.
	SourceRedeliveryRefusal string

	// SourceBestEffortTopics carries the typed adapter's best-effort direct_hold
	// admission. It never implies CapSourceRedelivery or waives failure sinks.
	// SourceReceiverID identifies the receiver in the activation disclosure.
	SourceBestEffortTopics []string
	SourceReceiverID       string

	// SourceSessionID is the id of the session this route's receiver subscribes
	// through, when the source is a stateful transport. The runtime installs the
	// ingress settlement barrier for this route on that session, so a session
	// managed only for its receivers (RegisterIngressSession) waits for the
	// deliveries the route accepted to settle before it recycles a broker
	// connection. It also names this route on a removed-subscription
	// dead-letter record, which a manual or automatic redrive of that record
	// needs (ADR 0019). Populated by the builder. A programmatic caller's route
	// session argument covers the barrier by identity but not the redrive: that
	// argument can be an egress session, so the runtime never names a route
	// from it. A hand-wired route that leaves this empty gets such records with
	// no route, and they stay in the DLQ for manual handling.
	SourceSessionID string
}

// CheckRandSource probes crypto/rand once and returns a permanent
// shared.BridgeError when the system random source is unavailable.
// Composition roots (e.g. bridge.Builder.Build) MUST call this once
// during wiring so a missing /dev/urandom (or equivalent) surfaces
// as a structured error before any envelope or instance ID is
// generated. The probe is cheap and idempotent: subsequent calls
// after a successful probe return nil without re-reading the source.
func CheckRandSource() error {
	if randProbeOK.Load() {
		return nil
	}
	b := make([]byte, 1)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return (&shared.BridgeError{
			Code:    shared.ErrCodeInternal,
			Class:   shared.ErrorPermanent,
			Message: "runtime: crypto/rand unavailable",
		}).Wrap(err)
	}
	randProbeOK.Store(true)
	return nil
}

// randProbeOK records that CheckRandSource has succeeded at least once
// in this process. It also tells generateID that the eager composition
// probe ran and the panic-free fallback below should remain dormant.
//
//nolint:gochecknoglobals // probe state must outlive every call
var randProbeOK atomic.Bool

// idFallbackCounter feeds the deterministic-but-unique fallback path in
// generateID when crypto/rand fails after composition (effectively
// impossible on supported OSes but kept as defence-in-depth so the
// runtime never panics on ID generation).
//
//nolint:gochecknoglobals // counter must outlive every call
var idFallbackCounter atomic.Uint64

func generateID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fallbackID()
	}
	return hex.EncodeToString(b)
}

// fallbackID returns a 32-hex-character ID assembled from the
// nanosecond clock (via clock.System, the documented default) and a
// process-local atomic counter. It is reachable only when crypto/rand
// fails post-composition; CheckRandSource is the supported way to
// surface a missing entropy source as a structured error at startup.
func fallbackID() string {
	n := idFallbackCounter.Add(1)
	ts := uint64(clock.System.Now().UnixNano())
	return strconv.FormatUint(ts, 16) + "-" + strconv.FormatUint(n, 16)
}
