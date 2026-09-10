// Deterministic unit tests for the second-round production-readiness
// audit fixes. No time.Sleep: races are driven with
// channels/cancellation and settlement failures are injected directly.
package amqp10

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/Azure/go-amqp"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// --- tls.enable on a non-TLS scheme is a cleartext downgrade trap ---

func TestSessionOptions_Validate_TLSEnableRequiresTLSScheme_F1(t *testing.T) {
	// The downgrade trap: tls.enable=true but a plaintext amqp:// address.
	// go-amqp applies TLSConfig only for amqps/amqp+ssl, so this would
	// dial (and send SASL PLAIN) in cleartext while Health reports Full.
	trap := SessionOptions{Address: "amqp://broker:5672", TLS: &TLSConfig{Enable: true}}
	err := trap.validate(false)
	require.Error(t, err, "tls.enable on amqp:// must be rejected")
	require.Contains(t, err.Error(), "cleartext")

	// A schemeless address is treated as plaintext too (mirrors go-amqp).
	noScheme := SessionOptions{Address: "broker:5672", TLS: &TLSConfig{Enable: true}}
	require.Error(t, noScheme.validate(false), "tls.enable on a schemeless address must be rejected")

	// The accepted TLS schemes validate.
	for _, addr := range []string{"amqps://broker:5671", "amqp+ssl://broker:5671"} {
		ok := SessionOptions{Address: addr, TLS: &TLSConfig{Enable: true}}
		require.NoError(t, ok.validate(false), "addr %q must validate with tls.enable", addr)
	}

	// tls.enable=false on a plaintext address is not a downgrade.
	off := SessionOptions{Address: "amqp://broker:5672", TLS: &TLSConfig{Enable: false}}
	require.NoError(t, off.validate(false))
}

// --- a plan with fewer active receivers than wanted is Degraded ---

func TestSession_Health_UnderProvisioned_Degraded_F5(t *testing.T) {
	// A plan wants two subscriptions but no receiver has attached yet:
	// active (0) < wanted (2). Reporting Full here hides the missing
	// routes from readiness — requires Degraded.
	s := newTestSession()
	s.mu.Lock()
	s.conn = &mockConn{}
	s.connected = true
	s.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "a"}, {Topic: "b"}},
	}
	s.mu.Unlock()

	h := s.Health(context.Background())
	require.True(t, h.Connected)
	require.Equal(t, 2, h.SubscriptionsWanted)
	require.Equal(t, 0, h.SubscriptionsActive)
	require.Equal(t, ports.ServiceLevelDegraded, h.ServiceLevel,
		"connected with active < wanted must be Degraded, not Full")
}

// --- an inbound ttl-only message gets a concrete egress expiry ---

func TestMessageToEnvelope_TTLPropagation_F7(t *testing.T) {
	base := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	abs := base.Add(2 * time.Hour)
	ttl := 45 * time.Second

	tests := []struct {
		name string
		msg  *amqp.Message
		want time.Time
	}{
		{
			name: "ttl only -> receiveTime + ttl",
			msg: &amqp.Message{
				Header: &amqp.MessageHeader{TTL: ttl},
				Data:   [][]byte{[]byte("body")},
			},
			want: base.Add(ttl),
		},
		{
			name: "absolute expiry wins over ttl",
			msg: &amqp.Message{
				Header:     &amqp.MessageHeader{TTL: ttl},
				Properties: &amqp.MessageProperties{AbsoluteExpiryTime: &abs},
				Data:       [][]byte{[]byte("body")},
			},
			want: abs,
		},
		{
			name: "neither -> no expiry",
			msg: &amqp.Message{
				Data: [][]byte{[]byte("body")},
			},
			want: time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := clocktest.NewAt(base)
			env, err := messageToEnvelope(tt.msg, clk)
			require.NoError(t, err)
			require.True(t, env.ExpiresAt().Equal(tt.want),
				"ExpiresAt = %v, want %v", env.ExpiresAt(), tt.want)
		})
	}
}

// --- a Done-driven connection loss marks senders down (symmetric) ---

func TestSession_HandleConnLost_MarksSendersDown_F8(t *testing.T) {
	s := newTestSession()
	mc := &mockConn{}
	s.mu.Lock()
	s.conn = mc
	s.connected = true
	s.mu.Unlock()

	snd := &Sender{}
	s.registerSender(snd)
	s.markSenderLink(snd, true)

	// A cancelled ctx makes handleReconnect return immediately (no re-dial
	// that would re-establish and clear the down state), so the down-marking
	// is observable deterministically without racing.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.handleConnLost(ctx, mc)

	s.mu.Lock()
	up := s.senders[snd]
	s.mu.Unlock()
	require.False(t, up, "sender must be marked down after a Done-driven connection loss")
}

// --- typed / binary message-ids render to clean strings in headers ---

func TestMessageToHeaders_RendersTypedIDsToStrings_F9(t *testing.T) {
	uuid := amqp.UUID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	corr := []byte{0xde, 0xad, 0xbe, 0xef}
	msg := &amqp.Message{Properties: &amqp.MessageProperties{
		MessageID:     uuid, // SDK amqp.UUID
		CorrelationID: corr, // SDK binary id
	}}

	h := messageToHeaders(msg)

	// ACL purity: no go-amqp SDK type or raw []byte may reach domain
	// headers (audit JSON would otherwise render raw byte arrays).
	for k, v := range h {
		switch v.(type) {
		case amqp.UUID, []byte:
			t.Fatalf("header %q leaked SDK/binary type %T into domain headers", k, v)
		}
	}
	require.Equal(t, uuid.String(), h[headerMessageID])
	require.Equal(t, hex.EncodeToString(corr), h[headerCorrelationID])
}

// --- max_frame_size below the AMQP 1.0 minimum (512) is rejected ---

func TestSessionOptions_Validate_MaxFrameSizeMinimum_F11(t *testing.T) {
	for _, sz := range []uint32{1, 100, 511} {
		o := SessionOptions{Address: "amqp://h", MaxFrameSize: sz}
		require.Error(t, o.validate(false), "max_frame_size %d (< 512) must be rejected", sz)
	}
	// 0 means "unset" (applyDefaults supplies the real default); >=512 ok.
	for _, sz := range []uint32{0, 512, 131072} {
		o := SessionOptions{Address: "amqp://h", MaxFrameSize: sz}
		require.NoError(t, o.validate(false), "max_frame_size %d must validate", sz)
	}
}

// --- group-sequence above MaxUint32 is dropped, not wrapped ---

func TestHeadersToMessage_GroupSequenceOverflow_F12(t *testing.T) {
	// In-range int is applied.
	in := headersToMessage(map[string]any{headerGroupSequence: 42})
	require.NotNil(t, in.Properties)
	require.NotNil(t, in.Properties.GroupSequence)
	require.Equal(t, uint32(42), *in.Properties.GroupSequence)

	// Above MaxUint32 must be dropped (never silently truncated to a small
	// wrong sequence number).
	over := headersToMessage(map[string]any{headerGroupSequence: int(math.MaxUint32) + 1})
	if over.Properties != nil && over.Properties.GroupSequence != nil {
		t.Fatalf("out-of-range group-sequence must be dropped, got %d", *over.Properties.GroupSequence)
	}

	// Negative int is dropped too (would wrap to a huge uint32).
	neg := headersToMessage(map[string]any{headerGroupSequence: -1})
	if neg.Properties != nil && neg.Properties.GroupSequence != nil {
		t.Fatalf("negative group-sequence must be dropped, got %d", *neg.Properties.GroupSequence)
	}
}

// --- resource-deleted / link:stolen map to permanent errors ---

func TestMapError_PermanentConditions_F13(t *testing.T) {
	tests := []struct {
		cond     string
		wantCode shared.ErrorCode
	}{
		{"amqp:resource-deleted", shared.ErrCodeNotFound},
		{"amqp:link:stolen", shared.ErrCodeForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.cond, func(t *testing.T) {
			got := MapError(&amqp.Error{Condition: amqp.ErrCond(tt.cond), Description: "d"})
			require.NotNil(t, got)
			require.Equal(t, tt.wantCode, got.Code)
			require.False(t, shared.IsRecoverableError(got),
				"%s must be permanent (non-retryable) so the link stops re-attaching forever", tt.cond)
			require.Equal(t, tt.cond, got.Context["condition"])
		})
	}
}

// --- (regression): a bare-int YAML duration is rejected by the real
// production decode path, not silently read as nanoseconds. The decode
// itself lives upstream in the shared config/parser hook chain; this
// test guards the amqp10 side against regressing past it. ---

func TestPluginOptionsDecode_BareIntDuration_Rejected_F6(t *testing.T) {
	input := map[string]any{
		"session": map[string]any{
			"address":         "amqp://localhost:5672",
			"connect_timeout": 30, // bare int, NOT "30s"
		},
	}
	var cfg Config
	err := parser.NewRawConfig(input).Decode(&cfg)
	require.Error(t, err, "a bare-int duration must be rejected, not decoded as 30ns")
	require.Contains(t, err.Error(), "bare number")
}

// --- a Sender is unusable after Close and does not re-register ---

func TestSender_SendAfterClose_ReturnsClosedSentinel_F14(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())
	s, err := NewSender(SenderConfig{Address: "queue/out"}, sess)
	require.NoError(t, err)

	require.NoError(t, s.Close(context.Background()))

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Subject: "t", Payload: []byte("d")})
	err = s.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	require.Error(t, err, "Send after Close must fail")
	require.True(t, errors.Is(err, shared.ErrTransportClosedPermanently),
		"want ErrTransportClosedPermanently, got %v", err)

	// The late Send must not re-attach a link and re-enter session health.
	sess.mu.Lock()
	_, present := sess.senders[s]
	sess.mu.Unlock()
	require.False(t, present, "closed sender must not re-enter the session health map")
}

// --- repeated failed settlements on a live link force a rebuild and
// emit an observability metric, instead of silently leaking link credit. ---

func TestReceiver_FailedSettlements_ForceLinkRebuild_F2(t *testing.T) {
	rec := &ports.RecordingExporter{}
	// LinkCredit 4 -> threshold 2: the second failure forces a rebuild
	// BEFORE all four credit slots are leaked and the receiver stalls.
	r, err := NewReceiver(ReceiverConfig{
		Address:    "queue/settle",
		LinkCredit: 4,
		Metrics:    rec,
	}, nil) // nil session: non-durable, closes the link directly on rebuild
	require.NoError(t, err)

	fl := &fakeLink{}
	r.mu.Lock()
	r.link = fl
	r.mu.Unlock()

	settler := newMockSettler()
	settler.acceptErr = errors.New("settle boom")

	for i := 0; i < 2; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID: "m", Subject: "t", Payload: []byte("d"),
		})
		del := NewDelivery(env, &amqp.Message{}, settler, nil, rec, nil)
		r.trackDelivery(del)
		_ = del.Ack(context.Background()) // fails -> onSettleFailed fires
	}

	require.Equal(t, 1, fl.closeCalls,
		"reaching the failed-settle threshold must force exactly one link rebuild")
	require.Len(t, rec.FindEntries(MetricAMQP10SettleFailed), 2,
		"every failed settlement must emit the observability metric")
}

func TestReceiver_FailedSettlements_DurableDropsConnection_F2(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sess := newTestSession()
	mc := &mockConn{}
	sess.mu.Lock()
	sess.conn = mc
	sess.connected = true
	sess.mu.Unlock()

	// LinkCredit 2 -> threshold 1: the first failure trips the rebuild.
	// A durable subscription link must NOT be closed (brokers read that as
	// UNSUBSCRIBE); the connection is dropped instead so the monitor
	// reconnects with the subscription intact.
	r, err := NewReceiver(ReceiverConfig{
		Address:        "queue/durable",
		LinkCredit:     2,
		DurabilityMode: 1,
		Metrics:        rec,
	}, sess)
	require.NoError(t, err)

	fl := &fakeLink{}
	r.mu.Lock()
	r.link = fl
	r.linkConn = mc
	r.mu.Unlock()

	settler := newMockSettler()
	settler.acceptErr = errors.New("settle boom")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m", Subject: "t", Payload: []byte("d")})
	del := NewDelivery(env, &amqp.Message{}, settler, nil, rec, nil)
	r.trackDelivery(del)
	_ = del.Ack(context.Background())

	require.Equal(t, 0, fl.closeCalls, "a durable link must NOT be closed (that is UNSUBSCRIBE)")
	mc.mu.Lock()
	closed := mc.closed
	mc.mu.Unlock()
	require.True(t, closed, "durable rebuild must drop the connection so the subscription survives")
	require.Len(t, rec.FindEntries(MetricAMQP10SettleFailed), 1)
}

// --- Close winning the Start race must not leak an immortal monitor
// goroutine, and Start must return the closed error (never nil). ---

// blockingConnectExporter parks Start inside the post-connect
// ConnectLatency Timer — after connect() has stored s.conn but BEFORE the
// monitor is installed — the exact race window. All other metrics are
// discarded via the embedded NoopExporter.
type blockingConnectExporter struct {
	ports.NoopExporter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingConnectExporter) Timer(name string, _ time.Duration, _ ...shared.Tag) {
	if name == MetricAMQP10ConnectLatency {
		b.once.Do(func() { close(b.entered) })
		<-b.release
	}
}

func TestSession_StartClose_Race_NoImmortalMonitor_F3(t *testing.T) {
	be := &blockingConnectExporter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	opts := SessionOptions{
		Address:        "amqp://localhost:5672",
		ConnectTimeout: 2 * time.Second,
		ReconnectDelay: 100 * time.Millisecond,
	}
	s := NewSession(opts, connectivity.SessionPersistent, slog.Default(), be)
	s.dial = mockDialFunc(&mockConn{}, nil)

	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(context.Background()) }()

	// Start has dialed, stored s.conn, and is now parked in the
	// ConnectLatency Timer just before the monitor install.
	<-be.entered

	// Close wins the race.
	require.NoError(t, s.Close(context.Background()))

	// Let Start proceed to the monitor-install re-check.
	close(be.release)

	err := <-startErr
	require.Error(t, err, "Start must not return nil for a session closed mid-start")
	require.True(t, errors.Is(err, shared.ErrTransportClosedPermanently),
		"Start error must wrap ErrTransportClosedPermanently, got %v", err)
}
