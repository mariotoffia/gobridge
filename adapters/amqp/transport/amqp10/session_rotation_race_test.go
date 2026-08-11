package amqp10

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ── Finding 2: connect()/ApplyCredentials data race ──────────────────

// TestSession_ConnectRotation_NoRace drives concurrent reconnect dials
// and credential rotations. The dial reads the TLS material the way
// defaultDial → BuildTLSConfig does, so before the fix (connect() read
// s.opts outside the lock and ApplyCredentials mutated the shared
// *TLSConfig in place) the race detector flags a torn cert/key read.
// The fix snapshots opts under s.mu in connect() and swaps a fresh
// *TLSConfig pointer in ApplyCredentials, so this is -race clean.
func TestSession_ConnectRotation_NoRace(t *testing.T) {
	s := newTestSession()
	s.mu.Lock()
	s.opts.TLS = &TLSConfig{
		Enable:  true,
		CertPEM: shared.NewSecret("cert-0"),
		KeyPEM:  shared.NewSecret("key-0"),
	}
	s.mu.Unlock()

	s.dial = func(_ context.Context, opts SessionOptions, creds amqp10Credentials) (amqpConn, error) {
		if opts.TLS != nil {
			_ = opts.TLS.CertPEM.Reveal()
			_ = opts.TLS.KeyPEM.Reveal()
			_ = opts.TLS.CACertPEM.Reveal()
			_ = opts.TLS.InsecureSkipVerify
		}
		_ = creds.Username
		return &mockConn{}, nil
	}
	defer func() { _ = s.Close(context.Background()) }()

	const iterations = 300
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = s.connect(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			set := connectivity.NewCredentialSet(nil, tlsMat(
				fmt.Sprintf("cert-%d", i+1),
				fmt.Sprintf("key-%d", i+1),
				[]string{fmt.Sprintf("ca-%d", i+1)},
				false))
			_ = s.ApplyCredentials(context.Background(), set)
		}
	}()
	wg.Wait()
}

// TestApplyAMQP10TLSMaterial_SwapsPointer verifies the helper replaces
// the *TLSConfig pointer (finding 2) rather than mutating the existing
// struct in place, and preserves file-based fields across the swap.
func TestApplyAMQP10TLSMaterial_SwapsPointer(t *testing.T) {
	orig := &TLSConfig{
		Enable:     true,
		CACertFile: "/etc/ca.pem",
		CertPEM:    shared.NewSecret("old-cert"),
		KeyPEM:     shared.NewSecret("old-key"),
	}
	tls := orig
	changed := applyAMQP10TLSMaterial(&tls, tlsMat("new-cert", "new-key", []string{"new-ca"}, false))
	if !changed {
		t.Fatal("applyAMQP10TLSMaterial should report a change")
	}
	if tls == orig {
		t.Fatal("pointer was not swapped; helper mutated the config in place (finding 2)")
	}
	if orig.CertPEM.Reveal() != "old-cert" {
		t.Fatalf("original config was mutated: CertPEM = %q", orig.CertPEM.Reveal())
	}
	if tls.CertPEM.Reveal() != "new-cert" || tls.KeyPEM.Reveal() != "new-key" {
		t.Fatalf("new config missing rotated material: %+v", tls)
	}
	if tls.CACertFile != "/etc/ca.pem" {
		t.Fatalf("file-based field not preserved across swap: CACertFile = %q", tls.CACertFile)
	}
}

// ── Finding 3: receiver pins sender-settle-mode ──────────────────────

// TestReceiverLinkOptions_PinsUnsettled verifies the receiver requests
// SenderSettleModeUnsettled so a broker downgrading to settled (pre-
// settled → at-most-once) fails the attach loudly instead of silently
// weakening the at-least-once guarantee.
func TestReceiverLinkOptions_PinsUnsettled(t *testing.T) {
	opts := receiverLinkOptions(10, 0, "queue", "")
	if opts.RequestedSenderSettleMode == nil {
		t.Fatal("RequestedSenderSettleMode is nil; broker settle-mode is unpinned (finding 3)")
	}
	if *opts.RequestedSenderSettleMode != amqp.SenderSettleModeUnsettled {
		t.Fatalf("RequestedSenderSettleMode = %v, want Unsettled", *opts.RequestedSenderSettleMode)
	}
}

// TestReceiverLinkOptions_DurableStillPinsUnsettled ensures the settle
// mode is pinned regardless of durability configuration.
func TestReceiverLinkOptions_DurableStillPinsUnsettled(t *testing.T) {
	opts := receiverLinkOptions(5, 2, "topic", "sub-1")
	if opts.RequestedSenderSettleMode == nil ||
		*opts.RequestedSenderSettleMode != amqp.SenderSettleModeUnsettled {
		t.Fatalf("durable receiver did not pin Unsettled: %v", opts.RequestedSenderSettleMode)
	}
	if opts.Name != "sub-1" {
		t.Fatalf("link name = %q, want sub-1", opts.Name)
	}
	if opts.SourceExpiryPolicy != amqp.ExpiryPolicyNever {
		t.Fatalf("durable source expiry = %v, want Never", opts.SourceExpiryPolicy)
	}
}

// ── Finding 4: SendBatch attach is bounded ───────────────────────────

// TestSender_SendBatch_AttachBounded verifies SendBatch bounds the
// initial link attach with cfg.Timeout (finding 4). The injected attach
// blocks until its context is done; before the fix ensureLink received
// the raw (deadline-less) caller context and the call would hang
// forever. With the fix the attach context carries cfg.Timeout, so the
// batch returns a timeout error.
func TestSender_SendBatch_AttachBounded(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost"}, connectivity.SessionEphemeral, slog.Default())
	s, err := NewSender(SenderConfig{Address: "queue/out", Timeout: 50 * time.Millisecond}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	// Attach blocks until the (bounded) context fires. If SendBatch does
	// not bound the attach, ctx is context.Background() and this never
	// returns → test times out.
	s.attach = func(ctx context.Context) (senderLinkAPI, amqpConn, error) {
		<-ctx.Done()
		return nil, nil, MapError(ctx.Err())
	}

	msgs := []ports.OutboundMessage{
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "b-1", Payload: []byte("one")})},
	}

	done := make(chan struct{})
	var results []ports.BatchResult
	var batchErr error
	go func() {
		results, batchErr = s.SendBatch(context.Background(), msgs)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SendBatch did not return; attach was not bounded (finding 4)")
	}

	if batchErr == nil {
		t.Fatalf("SendBatch err = nil, want a timeout error; results = %v", results)
	}
	be, ok := shared.AsBridgeError(batchErr)
	if !ok || be.Code != shared.ErrCodeTimeout {
		t.Fatalf("SendBatch err = %v, want ErrTimeout", batchErr)
	}
}

// ── Finding 6: typed message-id preserved ────────────────────────────

func TestMessageIDToString(t *testing.T) {
	uuid := amqp.UUID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string", "id-123", "id-123"},
		{"uuid", uuid, uuid.String()},
		{"ulong", uint64(1234567890), "1234567890"},
		{"binary", []byte{0xde}, "de"},
		{"unknown", 3.14, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageIDToString(tc.in); got != tc.want {
				t.Fatalf("messageIDToString(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReceiver_ConvertMessage_UUIDMessageID_Deterministic verifies a
// non-string (uuid) message-id yields a DETERMINISTIC envelope ID (its
// canonical rendering), not a random one (finding 6), so downstream
// message-id dedup survives.
func TestReceiver_ConvertMessage_UUIDMessageID_Deterministic(t *testing.T) {
	uuid := amqp.UUID{0xaa, 0xbb, 0xcc, 0xdd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01}
	newMsg := func() *amqp.Message {
		return &amqp.Message{
			Properties: &amqp.MessageProperties{MessageID: uuid},
			Data:       [][]byte{[]byte("body")},
		}
	}
	env1, err := messageToEnvelope(newMsg(), nil)
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}
	env2, err := messageToEnvelope(newMsg(), nil)
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}
	if env1.ID() != uuid.String() {
		t.Fatalf("envelope ID = %q, want deterministic uuid rendering %q", env1.ID(), uuid.String())
	}
	if env1.ID() != env2.ID() {
		t.Fatalf("envelope ID is not deterministic: %q != %q", env1.ID(), env2.ID())
	}
}

// TestEnvelope_RoundTrip_RendersTypedMessageIDToString verifies a uuid
// message-id survives ingress→egress as its DETERMINISTIC STRING
// rendering: (ACL purity) requires messageToHeaders to render typed
// ids via messageIDToString so no go-amqp SDK type reaches the domain
// headers, and envelopeToMessage must emit that string (not clobber it
// with a fresh envelope ID). Dedup is preserved because the rendering is
// stable. This supersedes the "preserve the typed amqp.UUID"
// contract (finding 6), which leaked an SDK type into the envelope.
func TestEnvelope_RoundTrip_RendersTypedMessageIDToString(t *testing.T) {
	uuid := amqp.UUID{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00}
	in := &amqp.Message{
		Properties: &amqp.MessageProperties{MessageID: uuid},
		Data:       [][]byte{[]byte("payload")},
	}
	env, err := messageToEnvelope(in, nil)
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}

	// ACL purity: the header must be the string rendering, never the
	// amqp.UUID SDK type.
	if got, ok := env.Headers()[headerMessageID].(string); !ok || got != uuid.String() {
		t.Fatalf("header message-id = %v (%T), want string %q",
			env.Headers()[headerMessageID], env.Headers()[headerMessageID], uuid.String())
	}

	out := envelopeToMessage(env, false)
	if out.Properties == nil || out.Properties.MessageID == nil {
		t.Fatal("egress message lost its message-id")
	}
	got, ok := out.Properties.MessageID.(string)
	if !ok {
		t.Fatalf("egress message-id type = %T, want string (ACL purity)", out.Properties.MessageID)
	}
	if got != uuid.String() {
		t.Fatalf("egress message-id = %q, want deterministic rendering %q", got, uuid.String())
	}
}

// TestEnvelope_Egress_StampsIDWhenNoMessageID verifies the envelope ID
// still stamps the message-id when no typed id was carried through.
func TestEnvelope_Egress_StampsIDWhenNoMessageID(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "gen-id", Payload: []byte("x")})
	out := envelopeToMessage(env, false)
	if out.Properties == nil || out.Properties.MessageID != "gen-id" {
		t.Fatalf("MessageID = %v, want stamped envelope ID", out.Properties.MessageID)
	}
}

// ── Finding 8: map config honors PEM keys ────────────────────────────

func TestSessionOptionsFromMap_HonorsPEMKeys(t *testing.T) {
	opts, err := SessionOptionsFromMap(map[string]any{
		"address": "amqps://localhost:5671",
		"tls": map[string]any{
			"enable":      true,
			"ca_cert_pem": "--CA--",
			"cert_pem":    "--CERT--",
			"key_pem":     "--KEY--",
		},
	})
	if err != nil {
		t.Fatalf("SessionOptionsFromMap: %v", err)
	}
	if opts.TLS == nil {
		t.Fatal("TLS config nil")
	}
	if opts.TLS.CACertPEM.Reveal() != "--CA--" {
		t.Fatalf("CACertPEM = %q, want --CA-- (finding 8: PEM keys dropped)", opts.TLS.CACertPEM.Reveal())
	}
	if opts.TLS.CertPEM.Reveal() != "--CERT--" {
		t.Fatalf("CertPEM = %q, want --CERT--", opts.TLS.CertPEM.Reveal())
	}
	if opts.TLS.KeyPEM.Reveal() != "--KEY--" {
		t.Fatalf("KeyPEM = %q, want --KEY--", opts.TLS.KeyPEM.Reveal())
	}
}

// ── Finding 9: SessionHealth.LastError + sender-link health ───────────

// TestSession_Health_LastErrorPopulatedOnDegrade verifies a down
// receiver link degrades the session AND surfaces the recorded cause via
// LastError (finding 9).
func TestSession_Health_LastErrorPopulatedOnDegrade(t *testing.T) {
	s := newTestSession()
	s.mu.Lock()
	s.conn = &mockConn{}
	s.connected = true
	s.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "a"}},
	}
	s.mu.Unlock()

	r := &Receiver{}
	s.registerReceiver(r)
	s.markReceiverLink(r, false) // link down
	s.noteLinkError(errors.New("boom: link detached"))

	h := s.Health(context.Background())
	if h.ServiceLevel != ports.ServiceLevelDegraded {
		t.Fatalf("ServiceLevel = %q, want Degraded", h.ServiceLevel)
	}
	if h.LastError == nil {
		t.Fatal("LastError is nil on a degraded session (finding 9)")
	}
}

// TestSession_Health_SenderSelfHealsAfterReconnect is the regression
// test for the finding-9 sender-health self-heal gap: a broker blip
// marks all registered senders down (notifyDisconnect →
// markAllSendersDownLocked), but a Sender has no background reattach
// path, so before the fix the session stayed ServiceLevelDegraded (with
// a misleading nil LastError, since connect() clears it) for the whole
// window until the next application Send. connect() now drops stale
// sender entries so a successful reconnect self-heals, and only a
// genuine send failure degrades.
func TestSession_Health_SenderSelfHealsAfterReconnect(t *testing.T) {
	s := newTestSession()
	// Fresh conn per dial so reconnect gets a live connection.
	s.dial = func(_ context.Context, _ SessionOptions, _ amqp10Credentials) (amqpConn, error) {
		return &mockConn{}, nil
	}
	defer func() { _ = s.Close(context.Background()) }()

	// Initial connect + a registered (healthy) sender link.
	if err := s.connect(context.Background()); err != nil {
		t.Fatalf("initial connect: %v", err)
	}
	sn := &Sender{}
	s.registerSender(sn)
	if h := s.Health(context.Background()); h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("pre-blip ServiceLevel = %q, want Full", h.ServiceLevel)
	}

	// Broker blip: notifyDisconnect marks every sender down and records
	// the cause, then a reconnect succeeds.
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	s.notifyDisconnect(conn, errors.New("connection reset by peer"))
	if err := s.connect(context.Background()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	// Self-heal: the benign post-reconnect window must NOT report a stale
	// sender-down degrade.
	h := s.Health(context.Background())
	if h.ServiceLevel == ports.ServiceLevelDegraded {
		t.Fatalf("post-reconnect ServiceLevel = Degraded due to stale sender-down; LastError = %v (want self-heal)", h.LastError)
	}
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("post-reconnect ServiceLevel = %q, want Full", h.ServiceLevel)
	}

	// A genuine send failure after reconnect still degrades correctly: the
	// next Send re-registers the link (createLink → registerSender), and
	// handleSendFailure marks it down.
	s.registerSender(sn)
	s.markSenderLink(sn, false)
	s.noteLinkError(errors.New("broker refused publish"))
	h = s.Health(context.Background())
	if h.ServiceLevel != ports.ServiceLevelDegraded {
		t.Fatalf("after real send failure ServiceLevel = %q, want Degraded", h.ServiceLevel)
	}
	if h.LastError == nil {
		t.Fatal("real send failure produced Degraded with nil LastError")
	}
}

// TestSession_Health_SenderLinkDownDegrades verifies a failing sender
// link degrades ServiceLevel even when the connection and all receivers
// are healthy (finding 9).
func TestSession_Health_SenderLinkDownDegrades(t *testing.T) {
	s := newTestSession()
	s.mu.Lock()
	s.conn = &mockConn{}
	s.connected = true
	s.mu.Unlock()

	sn := &Sender{}
	s.registerSender(sn)

	// Healthy sender → Full.
	if h := s.Health(context.Background()); h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("with healthy sender ServiceLevel = %q, want Full", h.ServiceLevel)
	}

	// Sender link fails → Degraded with cause.
	s.markSenderLink(sn, false)
	s.noteLinkError(errors.New("sender attach refused"))
	h := s.Health(context.Background())
	if h.ServiceLevel != ports.ServiceLevelDegraded {
		t.Fatalf("with down sender ServiceLevel = %q, want Degraded (finding 9)", h.ServiceLevel)
	}
	if h.LastError == nil {
		t.Fatal("LastError nil with a failing sender link")
	}
}
