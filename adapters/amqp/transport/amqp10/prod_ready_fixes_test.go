// Deterministic unit tests for the production-readiness fixes:
// durable outbound messages, disposition-aware send, durable
// subscription link naming, cold-start resilience, attach backoff,
// link-scoped error isolation, graceful Close with in-flight
// settlements, bridge-header identity lifting, creation-time
// preservation, delayed-retry annotation, ingress-reject isolation,
// routing string decode, and SASL mechanism selection.
package amqp10

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/go-amqp"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// waitFor polls cond (whitebox state observation) until it holds or the
// deadline expires. Used only to synchronise with goroutines the test
// itself started; production waits are driven by the fake clock.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for %s", msg)
		default:
		}
	}
}

// ── Finding 1: outbound durability ─────────────────────────────────

func TestEnvelopeToMessage_DurableHeader(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "d1", Payload: []byte("x")})

	t.Run("durable_sets_header", func(t *testing.T) {
		msg := envelopeToMessage(env, true)
		require.NotNil(t, msg.Header)
		require.True(t, msg.Header.Durable)
	})

	t.Run("non_durable_leaves_header_nil", func(t *testing.T) {
		msg := envelopeToMessage(env, false)
		require.Nil(t, msg.Header)
	})
}

func TestSenderConfig_DurableDefaultsTrue(t *testing.T) {
	var c SenderConfig
	require.True(t, c.durable(), "unset Durable must default to durable sends")

	f := false
	c.Durable = &f
	require.False(t, c.durable())

	tr := true
	c.Durable = &tr
	require.True(t, c.durable())
}

// ── Finding 6: disposition-aware send ──────────────────────────────

func TestDispositionError(t *testing.T) {
	tests := []struct {
		name      string
		state     amqp.DeliveryState
		wantNil   bool
		wantClass shared.ErrorClass
	}{
		{name: "accepted", state: &amqp.StateAccepted{}, wantNil: true},
		{name: "released_transient", state: &amqp.StateReleased{}, wantClass: shared.ErrorTransient},
		{name: "modified_transient", state: &amqp.StateModified{}, wantClass: shared.ErrorTransient},
		{name: "rejected_no_condition_rejected_class", state: &amqp.StateRejected{}, wantClass: shared.ErrorRejected},
		{
			name:      "rejected_condition_mapped",
			state:     &amqp.StateRejected{Error: &amqp.Error{Condition: amqp.ErrCondResourceLimitExceeded}},
			wantClass: shared.ErrorTransient,
		},
		{name: "unknown_state_transient", state: nil, wantClass: shared.ErrorTransient},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := dispositionError(tc.state)
			if tc.wantNil {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.ErrorIs(t, err, errNotAccepted,
				"non-accepted dispositions must carry the errNotAccepted marker")
			var be *shared.BridgeError
			require.ErrorAs(t, err, &be)
			require.Equal(t, tc.wantClass, be.Class)
			if tc.wantClass != shared.ErrorTransient {
				require.False(t, shared.IsRecoverableError(err),
					"non-transient dispositions must not be retried blindly")
			}
		})
	}
}

func TestSender_Send_NonAcceptedDisposition_KeepsLink(t *testing.T) {
	sess := newTestSession()
	s, err := NewSender(SenderConfig{Address: "queue/x", Session: sess}, sess)
	require.NoError(t, err)

	link := newMockSenderLink()
	close(link.sendBlock)
	link.sendBlock = nil
	link.sendErr = dispositionError(&amqp.StateReleased{})

	conn := &mockConn{}
	s.mu.Lock()
	s.link = link
	s.linkConn = conn
	s.mu.Unlock()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "rel-1", Payload: []byte("p")})
	sendErr := s.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	require.Error(t, sendErr)
	require.True(t, shared.IsRecoverableError(sendErr), "released must map transient")

	// The transfer reached the broker and it answered: the link is
	// HEALTHY. It must not be detached, and the connection must not be
	// torn down for a per-message outcome.
	s.mu.Lock()
	sameLink := s.link == senderLinkAPI(link)
	s.mu.Unlock()
	require.True(t, sameLink, "link must be kept after a non-accepted disposition")
	require.False(t, conn.closed, "connection must not be closed")
}

func TestSender_Send_TransportError_StillDetachesLink(t *testing.T) {
	sess := newTestSession()
	s, err := NewSender(SenderConfig{Address: "queue/x", Session: sess}, sess)
	require.NoError(t, err)

	link := newMockSenderLink()
	close(link.sendBlock)
	link.sendBlock = nil
	link.sendErr = errors.New("write: broken pipe")

	s.mu.Lock()
	s.link = link
	s.linkConn = &mockConn{}
	s.mu.Unlock()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "bp-1", Payload: []byte("p")})
	require.Error(t, s.Send(context.Background(), ports.OutboundMessage{Envelope: env}))

	s.mu.Lock()
	detached := s.link == nil
	s.mu.Unlock()
	require.True(t, detached, "transport errors must still detach the link")
}

// ── Finding 8: link-scoped vs connection-scoped errors ─────────────

func TestMapError_ScopedSDKErrors(t *testing.T) {
	t.Run("link_error_nil_remote_is_transient", func(t *testing.T) {
		be := MapError(&amqp.LinkError{})
		require.Equal(t, shared.ErrorTransient, be.Class)
		require.Equal(t, shared.ErrCodeUnavailable, be.Code)
	})
	t.Run("conn_error_nil_remote_is_connection_lost", func(t *testing.T) {
		be := MapError(&amqp.ConnError{})
		require.Equal(t, shared.ErrCodeConnectionLost, be.Code)
	})
	t.Run("session_error_nil_remote_is_transient", func(t *testing.T) {
		be := MapError(&amqp.SessionError{})
		require.Equal(t, shared.ErrorTransient, be.Class)
	})
	t.Run("bridge_error_passthrough", func(t *testing.T) {
		orig := shared.ErrInvalidPayload.WithMessage("amqp10: keep me")
		require.Same(t, orig, MapError(orig))
		require.Same(t, orig, MapError(fmt.Errorf("wrapped: %w", orig)))
	})
	t.Run("link_error_with_condition_maps_condition", func(t *testing.T) {
		be := MapError(&amqp.LinkError{RemoteErr: &amqp.Error{Condition: amqp.ErrCondUnauthorizedAccess}})
		require.Equal(t, shared.ErrCodeNotAuthorized, be.Code)
	})
}

func TestIsLinkScopedError(t *testing.T) {
	require.True(t, isLinkScopedError(&amqp.LinkError{}))
	require.True(t, isLinkScopedError(fmt.Errorf("recv: %w", &amqp.LinkError{})))
	require.False(t, isLinkScopedError(&amqp.ConnError{}))
	require.False(t, isLinkScopedError(&amqp.SessionError{}))
	require.False(t, isLinkScopedError(errors.New("amqp:link:detach-forced")))
	require.False(t, isLinkScopedError(nil))
}

func TestReceiver_HandleLinkError_LinkScoped_DoesNotTearDownConnection(t *testing.T) {
	sess := newTestSession()
	conn := &mockConn{}
	sess.mu.Lock()
	sess.conn = conn
	sess.connected = true
	sess.mu.Unlock()

	r, err := NewReceiver(ReceiverConfig{Address: "queue/in", Session: sess}, sess)
	require.NoError(t, err)

	fl := &fakeLink{}
	r.mu.Lock()
	r.link = fl
	r.linkConn = conn
	r.mu.Unlock()

	r.handleLinkError(&amqp.LinkError{}) // link-scoped: rebuild link only

	require.Equal(t, 1, fl.closeCalls, "faulted link must be closed")
	require.False(t, conn.closed, "connection must survive a link-scoped fault")
	sess.mu.Lock()
	stillConnected := sess.connected
	sess.mu.Unlock()
	require.True(t, stillConnected)

	// Contrast: a connection-scoped error must escalate.
	r.mu.Lock()
	r.link = &fakeLink{}
	r.linkConn = conn
	r.mu.Unlock()
	r.handleLinkError(&amqp.ConnError{})
	require.True(t, conn.closed, "conn-scoped fault must tear down the connection")
}

// ── Finding 3: cold start is not bridge-fatal ──────────────────────

func TestReceiver_Run_ColdStart_WaitsForSessionInsteadOfFailing(t *testing.T) {
	sess := newTestSession() // never Started: no conn, no amqp session
	r, err := NewReceiver(ReceiverConfig{Address: "queue/in", Session: sess}, sess)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(context.Context, ports.Delivery) error { return nil })
	}()

	// The buggy behaviour returned ErrUnavailable immediately; the fixed
	// receiver subscribes to session events and waits. Observe the
	// subscription appearing (whitebox), then cancel.
	waitFor(t, func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return len(sess.eventSubs) == 1
	}, "receiver to subscribe for session events")

	cancel()
	err = <-done
	require.ErrorIs(t, err, context.Canceled,
		"cold-start link failure must resolve to ctx cancellation, not a terminal transport error")
}

// ── Finding 5: backoff between failed link re-creations ────────────

func TestReceiver_ReceiveLoop_AttachFailure_BacksOff(t *testing.T) {
	fake := clocktest.NewAt(time.Unix(1_700_000_000, 0))

	sess := newTestSession()
	sess.mu.Lock()
	sess.conn = &mockConn{}
	sess.connected = true // connected, but amqpSess nil → attach always fails
	sess.mu.Unlock()

	r, err := NewReceiver(ReceiverConfig{
		Address: "queue/in",
		Session: sess,
		Clock:   fake,
	}, sess)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(context.Context, ports.Delivery) error { return nil })
	}()

	// First failed attach must arm a backoff timer instead of hot-spinning.
	waitFor(t, func() bool { return fake.TimerCount() >= 1 }, "first backoff timer")
	fake.Advance(2 * time.Second) // 1s ± 25% jitter

	// Second failed attach: the loop is still throttled.
	waitFor(t, func() bool { return fake.TimerCount() >= 1 }, "second backoff timer")

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// ── Findings 9/17: Close waits for in-flight settlements ───────────

func TestReceiver_Close_WaitsForInflightSettlement(t *testing.T) {
	r, err := NewReceiver(ReceiverConfig{Address: "queue/in"}, nil)
	require.NoError(t, err)
	fl := &fakeLink{}
	r.mu.Lock()
	r.link = fl
	r.mu.Unlock()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "close-1"})
	del := NewDelivery(env, &amqp.Message{}, newMockSettler(), slog.Default(), nil, nil)
	r.trackDelivery(del)

	closed := make(chan error, 1)
	go func() { closed <- r.Close(context.Background()) }()

	// Whitebox: Close must be parked on the in-flight wait, not the link.
	waitFor(t, func() bool {
		r.inflightMu.Lock()
		defer r.inflightMu.Unlock()
		return r.inflightIdle != nil
	}, "close to arm in-flight wait")
	require.Equal(t, 0, fl.closeCalls, "link must stay open while a settlement is in flight")

	require.NoError(t, del.Ack(context.Background()))

	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the settlement completed")
	}
	require.Equal(t, 1, fl.closeCalls, "link must be closed once settlements drained")
}

func TestReceiver_Close_BoundedByContext(t *testing.T) {
	r, err := NewReceiver(ReceiverConfig{Address: "queue/in"}, nil)
	require.NoError(t, err)
	fl := &fakeLink{}
	r.mu.Lock()
	r.link = fl
	r.mu.Unlock()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "close-2"})
	del := NewDelivery(env, &amqp.Message{}, newMockSettler(), slog.Default(), nil, nil)
	r.trackDelivery(del) // never settled

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-expired bound
	require.NoError(t, r.Close(ctx))
	require.Equal(t, 1, fl.closeCalls, "bounded Close must still detach the link")
}

func TestReceiver_EmitError_ReleasesInflightSlot(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "emit-err"})
	del := NewDelivery(env, &amqp.Message{}, newMockSettler(), slog.Default(), nil, nil)

	r, err := NewReceiver(ReceiverConfig{Address: "queue/in"}, nil)
	require.NoError(t, err)
	fl := &fakeLink{deliveries: []*Delivery{del}}
	r.mu.Lock()
	r.link = fl
	r.mu.Unlock()

	emitErr := errors.New("pipeline rejected delivery")
	runErr := r.Run(context.Background(), func(context.Context, ports.Delivery) error { return emitErr })
	require.ErrorIs(t, runErr, emitErr)

	// The pipeline never took ownership, so Close must not wait on it.
	require.NoError(t, r.Close(context.Background()))
	// The delivery itself must remain settleable by whoever holds it.
	require.NoError(t, del.Ack(context.Background()))
}

// ── Finding 17: concurrent settle reporting ────────────────────────

// blockingSettler parks AcceptMessage until released, so a test can
// observe the "in progress" state deterministically.
type blockingSettler struct {
	entered chan struct{}
	release chan struct{}
	err     error
}

func (b *blockingSettler) AcceptMessage(_ context.Context, _ *amqp.Message) error {
	close(b.entered)
	<-b.release
	return b.err
}
func (b *blockingSettler) ReleaseMessage(context.Context, *amqp.Message) error { return nil }
func (b *blockingSettler) ModifyMessage(context.Context, *amqp.Message, *amqp.ModifyMessageOptions) error {
	return nil
}

func TestDelivery_ConcurrentSettle_ReportsInProgress_NotFailed(t *testing.T) {
	bs := &blockingSettler{entered: make(chan struct{}), release: make(chan struct{})}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "conc-1"})
	del := NewDelivery(env, &amqp.Message{}, bs, slog.Default(), nil, nil)

	first := make(chan error, 1)
	go func() { first <- del.Ack(context.Background()) }()
	<-bs.entered // first settle is now in flight

	err := del.Ack(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "in progress",
		"a concurrent second settle must not be misreported as a previous failure")

	close(bs.release)
	require.NoError(t, <-first)

	// After a SUCCESSFUL settle, Ack stays idempotent-nil.
	require.NoError(t, del.Ack(context.Background()))
}

func TestDelivery_SettleAfterFailure_ReportsPreviousFailure(t *testing.T) {
	ms := newMockSettler()
	ms.acceptErr = errors.New("disposition write failed")
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "conc-2"})
	del := NewDelivery(env, &amqp.Message{}, ms, slog.Default(), nil, nil)

	require.Error(t, del.Ack(context.Background()))

	err := del.Retry(context.Background(), 0, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "previously failed")
}

// ── Finding 10: bridge-to-bridge identity headers lifted ───────────

func TestMessageToEnvelope_LiftsBridgeIdentityHeaders(t *testing.T) {
	msg := &amqp.Message{
		Data: [][]byte{[]byte("payload")},
		ApplicationProperties: map[string]any{
			messaging.HeaderIdempotencyKey:  "idem-1",
			messaging.HeaderDeduplicationID: "dedup-1",
			messaging.HeaderOrderingKey:     "order-1",
		},
	}
	env, err := messageToEnvelope(msg, clock.System)
	require.NoError(t, err)

	// NewEnvelope stamps the first-class identity fields into their
	// reserved headers via the trusted path (caller-supplied x-bridge.*
	// headers are stripped; only the lifted EnvelopeInput fields survive).
	idem, _ := env.Header(messaging.HeaderIdempotencyKey)
	require.Equal(t, "idem-1", idem,
		"idempotency key must survive the bridge→broker→bridge hop")
	dedup, _ := env.Header(messaging.HeaderDeduplicationID)
	require.Equal(t, "dedup-1", dedup)
	ord, _ := env.Header(messaging.HeaderOrderingKey)
	require.Equal(t, "order-1", ord)
}

// ── Findings 14/16: receive latency + ingress reject isolation ─────

// seqLink yields a scripted sequence of (delivery, error) results and
// then blocks until ctx is cancelled.
type seqLink struct {
	mu         sync.Mutex
	results    []seqResult
	idx        int
	closeCalls int
}

type seqResult struct {
	del *Delivery
	err error
}

func (f *seqLink) Receive(ctx context.Context, _ *slog.Logger, _ ports.MetricsExporter, _ clock.Clock) (*Delivery, error) {
	f.mu.Lock()
	if f.idx < len(f.results) {
		res := f.results[f.idx]
		f.idx++
		f.mu.Unlock()
		return res.del, res.err
	}
	f.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *seqLink) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return nil
}

func TestReceiver_IngressReject_IsNonTerminal_AndCounted(t *testing.T) {
	rec := &ports.RecordingExporter{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "good-1"})
	good := NewDelivery(env, &amqp.Message{}, newMockSettler(), slog.Default(), rec, nil)

	link := &seqLink{results: []seqResult{
		{err: fmt.Errorf("%w: %w", errIngressRejected, errors.New("malformed envelope"))},
		{del: good},
	}}

	r, err := NewReceiver(ReceiverConfig{Address: "queue/in", Metrics: rec}, nil)
	require.NoError(t, err)
	r.mu.Lock()
	r.link = link
	r.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var emitted int
	runErr := r.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		emitted++
		cancel()
		return nil
	})
	require.ErrorIs(t, runErr, context.Canceled,
		"a rejected poison message must NOT terminate the receive loop")
	require.Equal(t, 1, emitted, "the loop must keep receiving after a reject")
	require.Len(t, rec.FindEntries(MetricAMQP10IngressRejected), 1)
}

// ── Finding 15: creation-time preservation ─────────────────────────

func TestCreationTime_PreservedAcrossRelay(t *testing.T) {
	produced := time.Unix(1_600_000_000, 0).UTC()

	// Ingress: the producer's creation-time becomes the envelope's
	// CreatedAt (not the bridge receive time).
	inbound := &amqp.Message{
		Data:       [][]byte{[]byte("p")},
		Properties: &amqp.MessageProperties{CreationTime: &produced},
	}
	env, err := messageToEnvelope(inbound, clock.System)
	require.NoError(t, err)
	require.True(t, env.CreatedAt().Equal(produced),
		"ingress must preserve the producer creation-time")

	// Egress: the relayed amqp10.creation-time header wins over the
	// envelope CreatedAt so the original timestamp survives the hop.
	outbound := envelopeToMessage(env, true)
	require.NotNil(t, outbound.Properties.CreationTime)
	require.True(t, outbound.Properties.CreationTime.Equal(produced),
		"egress must not clobber the producer creation-time with bridge receive time")
}

func TestCreationTime_FallsBackToEnvelopeCreatedAt(t *testing.T) {
	created := time.Unix(1_650_000_000, 0).UTC()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "ct-2", Payload: []byte("p"), CreatedAt: created,
	})
	msg := envelopeToMessage(env, true)
	require.NotNil(t, msg.Properties.CreationTime)
	require.True(t, msg.Properties.CreationTime.Equal(created))
}

// ── Finding 11: delayed retry annotation ───────────────────────────

func TestDelivery_DelayedRetry_SetsDeliveryTimeAnnotation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	fake := clocktest.NewAt(now)
	ms := newMockSettler()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "delay-1"})
	del := NewDelivery(env, &amqp.Message{}, ms, slog.Default(), nil, fake)

	require.NoError(t, del.Retry(context.Background(), 30*time.Second, nil))

	require.Equal(t, 1, ms.modifyCalls)
	require.NotNil(t, ms.lastModifyOpts)
	require.True(t, ms.lastModifyOpts.DeliveryFailed)
	require.NotNil(t, ms.lastModifyOpts.Annotations)
	require.Equal(t, now.Add(30*time.Second).UnixMilli(),
		ms.lastModifyOpts.Annotations[annotationDeliveryTime],
		"x-opt-delivery-time must be the absolute scheduled redelivery time in ms")
}

func TestDelivery_ImmediateRetry_NoAnnotation(t *testing.T) {
	ms := newMockSettler()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "delay-0"})
	del := NewDelivery(env, &amqp.Message{}, ms, slog.Default(), nil, nil)

	require.NoError(t, del.Retry(context.Background(), 0, nil))
	require.Equal(t, 1, ms.releaseCalls, "immediate retry releases, no modified outcome")
	require.Equal(t, 0, ms.modifyCalls)
}

// ── Finding 2: durable subscription link naming ────────────────────

func TestReceiver_LinkName(t *testing.T) {
	newRecv := func(cfg ReceiverConfig, containerID string) *Receiver {
		sess := NewSession(SessionOptions{
			Address:     "amqp://localhost:5672",
			ContainerID: containerID,
		}, connectivity.SessionEphemeral, slog.Default())
		cfg.Session = sess
		r, err := NewReceiver(cfg, sess)
		require.NoError(t, err)
		return r
	}

	t.Run("explicit_subscription_name_wins", func(t *testing.T) {
		r := newRecv(ReceiverConfig{
			Address: "topic/a", DurabilityMode: 2, SubscriptionName: "orders-sub",
		}, "cid-1")
		require.Equal(t, "orders-sub", r.linkName())
	})

	t.Run("durable_derives_stable_name_from_container_id", func(t *testing.T) {
		r := newRecv(ReceiverConfig{Address: "topic/a", DurabilityMode: 2}, "cid-1")
		require.Equal(t, "cid-1:topic/a", r.linkName())
		require.Equal(t, r.linkName(), r.linkName(), "name must be deterministic")
	})

	t.Run("durable_without_container_id_uses_generated_instance_id", func(t *testing.T) {
		// Finding 16: an unset container_id is defaulted to a
		// per-instance "gobridge-<entropy>" id by applyDefaults, so the
		// derived link name is deterministic WITHIN the session (every
		// reconnect resumes the same durable subscription) while two
		// replicas never collide.
		r := newRecv(ReceiverConfig{Address: "topic/a", DurabilityMode: 2}, "")
		name := r.linkName()
		require.True(t, strings.HasPrefix(name, defaultContainerIDPrefix),
			"derived name must embed the generated per-instance container-id, got %q", name)
		require.True(t, strings.HasSuffix(name, ":topic/a"))
		require.Equal(t, name, r.linkName(), "name must be deterministic within the session")

		other := newRecv(ReceiverConfig{Address: "topic/a", DurabilityMode: 2}, "")
		require.NotEqual(t, name, other.linkName(),
			"two sessions without container_id must not share a durable subscription identity")
	})

	t.Run("non_durable_uses_sdk_random_name", func(t *testing.T) {
		r := newRecv(ReceiverConfig{Address: "queue/a"}, "cid-1")
		require.Empty(t, r.linkName())
	})
}

// ── Finding 12: routing string forms ───────────────────────────────

func TestRoutingType_UnmarshalText(t *testing.T) {
	tests := []struct {
		in      string
		want    RoutingType
		wantErr bool
	}{
		{in: "anycast", want: RoutingAnycast},
		{in: "multicast", want: RoutingMulticast},
		{in: "MULTICAST", want: RoutingMulticast},
		{in: " anycast ", want: RoutingAnycast},
		{in: "0", want: RoutingAnycast},
		{in: "1", want: RoutingMulticast},
		{in: "bogus", wantErr: true},
		{in: "2", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			var rt RoutingType
			err := rt.UnmarshalText([]byte(tc.in))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, rt)
		})
	}
}

// ── Finding 17: SASL mechanism knob ────────────────────────────────

func TestSessionOptions_ValidateSASLMechanism(t *testing.T) {
	// Mechanisms that need no client certificate validate with a plain
	// address.
	for _, ok := range []string{"", "plain", "anonymous"} {
		opts := SessionOptions{Address: "amqp://h", SASLMechanism: ok}
		require.NoError(t, opts.validate(false), "mechanism %q must validate", ok)
	}
	// F10: EXTERNAL (case-insensitive) authenticates via an mTLS client
	// certificate. It validates only with enabled TLS + client key-pair
	// material over an amqps endpoint, and is rejected without it.
	for _, ext := range []string{"external", "EXTERNAL"} {
		withCert := SessionOptions{
			Address:       "amqps://h",
			SASLMechanism: ext,
			TLS:           &TLSConfig{Enable: true, CertFile: "/c.pem", KeyFile: "/k.pem"},
		}
		require.NoError(t, withCert.validate(false), "external %q with client cert must validate", ext)

		noCert := SessionOptions{Address: "amqp://h", SASLMechanism: ext}
		err := noCert.validate(false)
		require.Error(t, err, "external %q without client cert must fail", ext)
		require.Contains(t, err.Error(), "external")

		// Deferred path (FIX 1): with a pending credentials_uri the same
		// cert-less EXTERNAL config passes parse-time validation; the check
		// moves to Config.ApplyCredentials post-resolution.
		require.NoError(t, noCert.validate(true),
			"external %q must defer the cert check while credentials pending", ext)
	}
	opts := SessionOptions{Address: "amqp://h", SASLMechanism: "gssapi"}
	err := opts.validate(false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sasl_mechanism")
}
