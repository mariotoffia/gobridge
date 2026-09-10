// Chunk 12 HIGH findings — regression tests.
//
//   - durable subscriptions silently lose continuity across a
//     process restart when the container_id is auto-generated. The factory
//     now fails closed unless an explicit session.container_id is set.
//   - malformed-ingress rejection ignored a FAILED settlement,
//     emitting a false "rejected" metric and leaving the link healthy. The
//     ACL now surfaces the settlement error so the receive loop rebuilds
//     the link and never counts the message as an ingress rejection.
//   - a durable receiver's close forces a full connection teardown
//     that blips every sibling link. The factory now enforces the
//     dedicated-session contract at build time.
//
// Every "rejected" / "OK" pair is a matched counterfactual so the gate is
// proven to fail closed WITHOUT over-enforcing on the safe topologies.
package amqp10

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/go-amqp"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

// factorySession builds a Session through the Factory (mirroring the
// runtime build path) so the container_id generation flag is populated
// exactly as it is in production.
func factorySession(t *testing.T, f *Factory, opts SessionOptions) *Session {
	t.Helper()
	sess, err := f.NewSession(context.Background(), ports.SessionSpec{
		ID:          "sess",
		SessionMode: connectivity.SessionEphemeral,
		Config:      Config{Session: opts},
	})
	require.NoError(t, err)
	amqpSess, ok := sess.(*Session)
	require.True(t, ok, "factory session type = %T, want *Session", sess)
	return amqpSess
}

// --- durable continuity requires an explicit container_id -------

func TestSessionOptions_ApplyDefaults_TracksGeneratedContainerID(t *testing.T) {
	gen := SessionOptions{Address: "amqp://localhost:5672"}
	gen.applyDefaults()
	require.True(t, gen.containerIDGenerated,
		"a synthesised container_id must be flagged so the factory can gate durable receivers")
	require.NotEmpty(t, gen.ContainerID, "applyDefaults must synthesise a container_id when none is set")

	explicit := SessionOptions{Address: "amqp://localhost:5672", ContainerID: "bridge-01"}
	explicit.applyDefaults()
	require.False(t, explicit.containerIDGenerated,
		"an operator-supplied container_id must not be flagged as generated")
	require.Equal(t, "bridge-01", explicit.ContainerID)
}

func TestFactory_NewReceiver_DurableWithoutExplicitContainerID_Rejected(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	// No container_id: applyDefaults synthesises one, which changes on
	// every process restart and cannot anchor a durable subscription.
	sess := factorySession(t, f, SessionOptions{Address: "amqp://localhost:5672"})

	_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "durable-orders",
		Config: Config{Receiver: ReceiverParams{Address: "topic/orders", DurabilityMode: 2}},
	}, sess)
	require.Error(t, err, "durable receiver on a generated container_id must fail closed")
	require.True(t, errors.Is(err, shared.ErrInvalidPayload),
		"rejection must be an invalid-payload build error, got %v", err)
}

func TestFactory_NewReceiver_DurableWithExplicitContainerID_OK(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	sess := factorySession(t, f, SessionOptions{Address: "amqp://localhost:5672", ContainerID: "bridge-01"})

	recv, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "durable-orders",
		Config: Config{Receiver: ReceiverParams{Address: "topic/orders", DurabilityMode: 2}},
	}, sess)
	require.NoError(t, err, "an explicit, stable container_id makes durable mode restart-safe")
	require.NotNil(t, recv)
}

func TestFactory_NewReceiver_NonDurableWithoutContainerID_OK(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	sess := factorySession(t, f, SessionOptions{Address: "amqp://localhost:5672"})

	recv, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "plain",
		Config: Config{Receiver: ReceiverParams{Address: "queue/in"}}, // durability_mode 0
	}, sess)
	require.NoError(t, err, "non-durable receivers must not be gated on container_id")
	require.NotNil(t, recv)
}

// --- malformed-ingress rejection must honour failed settlement ---

// fakeRawReceiver is a rawReceiver test double. It lives in a _test.go
// file, so importing the go-amqp SDK here does not breach the ACL rule
// that production SDK use is confined to acl_*.go. It returns a fixed
// message from Receive and a configurable error from RejectMessage, making
// the malformed-ingress settlement-failure path unit-testable
// without a live broker.
type fakeRawReceiver struct {
	msg         *amqp.Message
	recvErr     error
	rejectErr   error
	rejectCalls int32
}

var _ rawReceiver = (*fakeRawReceiver)(nil)

func (f *fakeRawReceiver) Receive(_ context.Context, _ *amqp.ReceiveOptions) (*amqp.Message, error) {
	if f.recvErr != nil {
		return nil, f.recvErr
	}
	return f.msg, nil
}

func (f *fakeRawReceiver) RejectMessage(_ context.Context, _ *amqp.Message, _ *amqp.Error) error {
	atomic.AddInt32(&f.rejectCalls, 1)
	return f.rejectErr
}

func (f *fakeRawReceiver) Address() string                                     { return "queue/poison" }
func (f *fakeRawReceiver) Close(context.Context) error                         { return nil }
func (f *fakeRawReceiver) AcceptMessage(context.Context, *amqp.Message) error  { return nil }
func (f *fakeRawReceiver) ReleaseMessage(context.Context, *amqp.Message) error { return nil }
func (f *fakeRawReceiver) ModifyMessage(context.Context, *amqp.Message, *amqp.ModifyMessageOptions) error {
	return nil
}

func TestReceiverLink_Receive_RejectFailure_ReturnsSettlementError(t *testing.T) {
	rejectErr := errors.New("broker settlement failed")
	raw := &fakeRawReceiver{
		// A non-string/[]byte amqp-value body is unrepresentable, so
		// messageToEnvelope fails and the reject path runs.
		msg:       &amqp.Message{Value: int64(7)},
		rejectErr: rejectErr,
	}
	link := &receiverLink{raw: raw}

	_, err := link.Receive(context.Background(), slog.Default(), nil, clock.System)
	require.Error(t, err)
	require.True(t, errors.Is(err, rejectErr),
		"a FAILED reject must surface the settlement error so the loop rebuilds the link; got %v", err)
	require.False(t, errors.Is(err, errIngressRejected),
		"an unsettled delivery must NOT be reported as a handled ingress rejection")
	require.Equal(t, int32(1), atomic.LoadInt32(&raw.rejectCalls))
}

func TestReceiverLink_Receive_RejectSuccess_ReturnsIngressRejected(t *testing.T) {
	raw := &fakeRawReceiver{
		msg:       &amqp.Message{Value: int64(7)},
		rejectErr: nil, // the reject settles cleanly
	}
	link := &receiverLink{raw: raw}

	_, err := link.Receive(context.Background(), slog.Default(), nil, clock.System)
	require.Error(t, err)
	require.True(t, errors.Is(err, errIngressRejected),
		"a cleanly-rejected malformed message is a handled per-message event")
	require.True(t, errors.Is(err, errUnrepresentableBody),
		"the underlying decode cause should stay wrapped for diagnostics")
	require.Equal(t, int32(1), atomic.LoadInt32(&raw.rejectCalls))
}

// TestReceiveLoop_SettlementFailure_MarksLinkDownAndSkipsRejectedMetric
// drives the real receive loop through the real ACL: a malformed message
// whose reject FAILS must route through handleLinkError (link marked down)
// and must NOT emit a false ingress-rejected metric. Pre-fix the ACL
// swallowed the reject error and returned errIngressRejected, so the loop
// counted the poison message as rejected and left the link healthy.
func TestReceiveLoop_SettlementFailure_MarksLinkDownAndSkipsRejectedMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sess := newTestSession()
	r, err := NewReceiver(ReceiverConfig{Address: "queue/poison", Metrics: rec}, sess)
	require.NoError(t, err)

	// Register and mark the link UP so a later down-flip is observable and
	// attributable to the settlement failure rather than the initial state.
	sess.registerReceiver(r)
	sess.markReceiverLink(r, true)

	r.mu.Lock()
	r.link = &receiverLink{raw: &fakeRawReceiver{
		msg:       &amqp.Message{Value: int64(42)}, // unrepresentable body
		rejectErr: context.DeadlineExceeded,        // reject (settlement) fails
	}}
	r.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.receiveLoop(ctx, func(context.Context, ports.Delivery) error { return nil })
	}()

	// handleLinkError runs once the settlement error surfaces and flips the
	// receiver link down. Poll for it (no sleeps).
	require.Eventually(t, func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		up, ok := sess.receivers[r]
		return ok && !up
	}, 2*time.Second, time.Millisecond,
		"a failed settlement must mark the receiver link down (unhealthy)")

	require.Empty(t, rec.FindEntries(MetricAMQP10IngressRejected),
		"a failed settlement must NOT be counted as an ingress rejection")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("receiveLoop did not exit after context cancel")
	}
}

// --- durable receiver requires a dedicated session --------------

func TestSession_ReserveLink_DurableRequiresDedicatedSession(t *testing.T) {
	t.Run("durable_alone_ok", func(t *testing.T) {
		s := newTestSession()
		require.NoError(t, s.reserveLink(true))
	})
	t.Run("sibling_then_durable_rejected", func(t *testing.T) {
		s := newTestSession()
		require.NoError(t, s.reserveLink(false)) // a sender / plain receiver
		err := s.reserveLink(true)               // durable receiver joins
		require.Error(t, err)
		require.True(t, errors.Is(err, shared.ErrInvalidPayload), "got %v", err)
	})
	t.Run("durable_then_sibling_rejected", func(t *testing.T) {
		s := newTestSession()
		require.NoError(t, s.reserveLink(true)) // durable claims the session
		err := s.reserveLink(false)             // sibling tries to join
		require.Error(t, err)
		require.True(t, errors.Is(err, shared.ErrInvalidPayload), "got %v", err)
	})
	t.Run("two_durables_rejected", func(t *testing.T) {
		s := newTestSession()
		require.NoError(t, s.reserveLink(true))
		require.Error(t, s.reserveLink(true))
	})
	t.Run("multiple_non_durable_ok", func(t *testing.T) {
		s := newTestSession()
		require.NoError(t, s.reserveLink(false))
		require.NoError(t, s.reserveLink(false))
		require.NoError(t, s.reserveLink(false))
	})
}

func TestFactory_NewReceiver_DurableOnSharedSession_Rejected(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	sess := factorySession(t, f, SessionOptions{Address: "amqp://localhost:5672", ContainerID: "bridge-01"})

	// A non-durable sibling is built first on the shared session.
	_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "plain",
		Config: Config{Receiver: ReceiverParams{Address: "queue/in"}},
	}, sess)
	require.NoError(t, err)

	// The durable receiver has an explicit container_id (so the Factory admits it) but
	// must still be rejected because it would share the session.
	_, err = f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "durable",
		Config: Config{Receiver: ReceiverParams{Address: "topic/orders", DurabilityMode: 1}},
	}, sess)
	require.Error(t, err, "durable receiver may not share a session")
	require.True(t, errors.Is(err, shared.ErrInvalidPayload), "got %v", err)
}

func TestFactory_DurableReceiverThenSender_Rejected(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	sess := factorySession(t, f, SessionOptions{Address: "amqp://localhost:5672", ContainerID: "bridge-01"})

	_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "durable",
		Config: Config{Receiver: ReceiverParams{Address: "topic/orders", DurabilityMode: 1}},
	}, sess)
	require.NoError(t, err, "durable receiver alone on its own session is allowed")

	_, err = f.NewSender(context.Background(), ports.SenderSpec{
		ID:     "publisher",
		Config: Config{Sender: SenderParams{Address: "queue/out"}},
	}, sess)
	require.Error(t, err, "a sender may not join a session already claimed by a durable receiver")
	require.True(t, errors.Is(err, shared.ErrInvalidPayload), "got %v", err)
}

func TestFactory_DurableReceiverOnDedicatedSession_OK(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	sess := factorySession(t, f, SessionOptions{Address: "amqp://localhost:5672", ContainerID: "bridge-01"})

	recv, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "durable",
		Config: Config{Receiver: ReceiverParams{Address: "topic/orders", DurabilityMode: 1}},
	}, sess)
	require.NoError(t, err, "a durable receiver alone on its own session is the sanctioned topology")
	require.NotNil(t, recv)
}

func TestFactory_NonDurableReceiverAndSender_SameSession_OK(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	sess := factorySession(t, f, SessionOptions{Address: "amqp://localhost:5672"})

	_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "plain-rx",
		Config: Config{Receiver: ReceiverParams{Address: "queue/in"}},
	}, sess)
	require.NoError(t, err)

	_, err = f.NewSender(context.Background(), ports.SenderSpec{
		ID:     "plain-tx",
		Config: Config{Sender: SenderParams{Address: "queue/out"}},
	}, sess)
	require.NoError(t, err, "non-durable receiver + sender may share a session")
}

// ── Additional cases ───────────────────────────────────────────────────
//
// These tests lock in the fixes for the adversarial follow-up review:
//   review-1: a durable settlement/transient fault must force a connection
//             teardown so the abandoned durable link's credit is reissued.
//   review-2: Factory is the production enforcement boundary; the low-level
//             constructors stay permissive by design.
//   review-3: a failed constructor must not leak a link reservation.

// TestReceiver_HandleLinkError_Durable_SettlementFailure_TearsDownConnection
// is the review-1 regression guard. A durable receiver rejecting a malformed
// message can hit a FAILED reject settlement, surfaced as a wrapped
// context.DeadlineExceeded. MapError classifies that as a TRANSIENT
// ErrTimeout — NOT ConnectionLost/Unavailable — so the generic non-durable
// escalation would leave the link stuck: the durable link stays attached on
// the wire (a closing detach would UNSUBSCRIBE), r.link is nil, and the
// broker never reissues the credit held for the poison delivery. The fix
// forces a full connection teardown for ANY durable link fault so the
// monitor reconnects and the link re-attaches with fresh credit.
//
// Mutation killed: drop the durable branch in handleLinkError (fall through
// to the ErrCodeConnectionLost/Unavailable-only escalation) → ErrTimeout no
// longer qualifies → conn stays open → require.True(closed) FAILs.
func TestReceiver_HandleLinkError_Durable_SettlementFailure_TearsDownConnection(t *testing.T) {
	sess := newTestSession()
	conn := &mockConn{}
	sess.mu.Lock()
	sess.conn = conn
	sess.connected = true
	sess.mu.Unlock()

	r, err := NewReceiver(ReceiverConfig{Address: "topic/durable", DurabilityMode: 2}, sess)
	require.NoError(t, err)
	sess.registerReceiver(r)
	sess.markReceiverLink(r, true)

	link := &recordingLink{}
	r.mu.Lock()
	r.link = link
	r.linkConn = conn
	r.mu.Unlock()

	// Exact shape: a failed reject settlement wraps
	// context.DeadlineExceeded, which maps to a transient ErrTimeout.
	settleErr := fmt.Errorf("amqp10: reject malformed inbound message: %w", context.DeadlineExceeded)
	require.Equal(t, shared.ErrCodeTimeout, MapError(settleErr).Code,
		"precondition: a failed reject surfaces as a transient timeout, not a connection loss")

	r.handleLinkError(settleErr)

	// The durable link must NEVER be link-closed — a closing detach is a
	// broker UNSUBSCRIBE that destroys the terminus and its retained state.
	link.mu.Lock()
	closeCalls := link.closeCalls
	link.mu.Unlock()
	require.Zero(t, closeCalls, "durable link must not be link-closed (would UNSUBSCRIBE)")

	// The connection MUST be torn down so the abandoned durable link dies
	// with it and a fresh link re-attaches with full credit on reconnect.
	conn.mu.Lock()
	closed := conn.closed
	conn.mu.Unlock()
	require.True(t, closed,
		"a durable transient/settlement fault must force a connection teardown so the stuck "+
			"link is abandoned and credit is reissued on reconnect (review-1)")

	sess.mu.Lock()
	connCleared := sess.conn == nil
	connected := sess.connected
	linkDown := !sess.receivers[r]
	sess.mu.Unlock()
	require.True(t, connCleared, "session connection must be cleared for re-dial")
	require.False(t, connected, "session must report not-connected after teardown")
	require.True(t, linkDown, "durable receiver link must be marked down")

	// The faulted durable link is abandoned locally so the reconnect builds
	// a FRESH link — and a fresh AMQP link starts with full credit
	// (createLink also resets the settlement-failure counter).
	r.mu.Lock()
	linkAbandoned := r.link == nil
	r.mu.Unlock()
	require.True(t, linkAbandoned, "faulted durable link must be abandoned so a fresh link relatches")

	// A reconnect is signalled so the monitor re-dials promptly.
	select {
	case <-sess.reconnectCh:
	default:
		t.Fatal("durable fault must signal a reconnect so the link relatches with fresh credit")
	}
}

// TestReceiver_HandleLinkError_NonDurable_TransientFault_KeepsConnection is
// the counterfactual: a NON-durable receiver hit by the SAME transient fault
// rebuilds only its own link and must NOT tear down the shared connection —
// proving the teardown in the test above is durable-specific and does not
// regress the isolated-rebuild path other links depend on.
func TestReceiver_HandleLinkError_NonDurable_TransientFault_KeepsConnection(t *testing.T) {
	sess := newTestSession()
	conn := &mockConn{}
	sess.mu.Lock()
	sess.conn = conn
	sess.connected = true
	sess.mu.Unlock()

	r, err := NewReceiver(ReceiverConfig{Address: "queue/plain"}, sess) // DurabilityMode 0
	require.NoError(t, err)

	fl := &fakeLink{}
	r.mu.Lock()
	r.link = fl
	r.linkConn = conn
	r.mu.Unlock()

	r.handleLinkError(fmt.Errorf("amqp10: reject malformed inbound message: %w", context.DeadlineExceeded))

	require.Equal(t, 1, fl.closeCalls, "non-durable link must be closed and rebuilt in isolation")
	conn.mu.Lock()
	closed := conn.closed
	conn.mu.Unlock()
	require.False(t, closed, "a transient non-durable fault must NOT tear down the shared connection")
	sess.mu.Lock()
	stillConnected := sess.connected
	sess.mu.Unlock()
	require.True(t, stillConnected, "session must stay connected after an isolated non-durable rebuild")
}

// TestFactory_NewReceiver_ConfigError_DoesNotLeakReservation is the review-3
// regression guard. An overflowing link_credit fails ReceiverConfig.validate
// INSIDE NewReceiver — which pre-fix ran AFTER reserveLink, leaking the
// reservation (builtLinkCount stayed incremented). A LATER durable receiver
// on the same session was then falsely rejected by the dedicated-session
// gate. The fix constructs+validates before reserving, so a failed build
// consumes no reservation.
//
// Mutation killed: restore the reserve-then-construct order → the leaked
// reservation trips the durable dedicated-session gate → the second
// NewReceiver returns an error → require.NoError FAILs.
func TestFactory_NewReceiver_ConfigError_DoesNotLeakReservation(t *testing.T) {
	f := &Factory{Logger: slog.Default()}
	sess := factorySession(t, f, SessionOptions{
		Address:     "amqp://localhost:5672",
		ContainerID: "bridge-01", // explicit → durable gate satisfied
	})

	// link_credit overflows int32 → rejected by ReceiverConfig.validate,
	// which runs inside NewReceiver (the pre-fix post-reservation point).
	_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "bad-credit",
		Config: Config{Receiver: ReceiverParams{Address: "queue/in", LinkCredit: math.MaxInt32 + 1}},
	}, sess)
	require.Error(t, err, "an overflowing link_credit must fail construction")
	require.True(t, errors.Is(err, shared.ErrInvalidPayload), "want ErrInvalidPayload, got %v", err)

	// The failed build must not have consumed a link reservation: a durable
	// receiver alone on this session is the sanctioned topology and must be
	// accepted. Pre-fix the leaked reservation made builtLinkCount>0, so the
	// dedicated-session gate falsely rejected this durable build.
	recv, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "durable",
		Config: Config{Receiver: ReceiverParams{Address: "topic/orders", DurabilityMode: 1}},
	}, sess)
	require.NoError(t, err, "a config error must not leak a link reservation onto the session (review-3)")
	require.NotNil(t, recv)
}

// TestLowLevelConstructor_PermissiveByDesign locks in the review-2 decision.
// Production builds every AMQP 1.0 link through Factory (the
// ports.TransportFactory interface — grep confirms no non-test caller invokes
// the package-level NewReceiver/NewSender/NewSession), so the durable safety
// gates live in the Factory and the low-level constructors stay PERMISSIVE:
// tests build durable links directly on shared sessions to exercise teardown
// behaviour. If a future change moves a gate into a low-level constructor,
// the first assertions here break and force a conscious re-decision (and a
// matching relocation of the test seams).
func TestLowLevelConstructor_PermissiveByDesign(t *testing.T) {
	// newTestSession synthesises a container_id (generated), which the
	// Factory's gate rejects for durable receivers.
	sess := newTestSession()
	require.True(t, sess.opts.containerIDGenerated, "precondition: session has a generated container_id")

	// Low-level NewReceiver is permissive: it accepts a durable receiver on
	// a generated-container_id session (which the Factory would reject).
	durable, err := NewReceiver(ReceiverConfig{Address: "topic/orders", DurabilityMode: 1}, sess)
	require.NoError(t, err, "low-level NewReceiver must stay permissive (durable gate lives in Factory)")
	require.NotNil(t, durable)

	// And it does NOT enforce the dedicated-session contract: a
	// second durable receiver on the same session is accepted low-level.
	durable2, err := NewReceiver(ReceiverConfig{Address: "topic/other", DurabilityMode: 1}, sess)
	require.NoError(t, err, "low-level NewReceiver must not enforce the dedicated-session contract")
	require.NotNil(t, durable2)

	// The SAME durable receiver built via the Factory IS rejected — proving
	// the Factory is the enforcement boundary that production always uses.
	f := &Factory{Logger: slog.Default()}
	fsess := factorySession(t, f, SessionOptions{Address: "amqp://localhost:5672"}) // generated cid
	_, ferr := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "durable",
		Config: Config{Receiver: ReceiverParams{Address: "topic/orders", DurabilityMode: 1}},
	}, fsess)
	require.Error(t, ferr, "Factory.NewReceiver MUST enforce the durable container_id gate")
	require.True(t, errors.Is(ferr, shared.ErrInvalidPayload), "want ErrInvalidPayload, got %v", ferr)
}
