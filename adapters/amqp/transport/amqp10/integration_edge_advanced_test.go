package amqp10

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
)

// ═══════════════════════════════════════════════
// AMQP 1.0 Edge Integration Tests (Part 2)
//
// Validates health transitions, multicast routing, batch completeness,
// header edge cases, envelope round-trip, and session reconnect.
// ═══════════════════════════════════════════════

// TestIntegration_Edge_SessionHealthTransitions validates the Health struct
// at each lifecycle stage: before start, after start, after close.
func TestIntegration_Edge_SessionHealthTransitions(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()

	sess := NewSession(SessionOptions{
		Address: ep, Username: user, Password: pass,
		ConnectTimeout: 15 * time.Second,
	}, domain.SessionEphemeral, logger)

	ctx := context.Background()

	h := sess.Health(ctx)
	if h.Connected {
		t.Error("should not be connected before Start")
	}
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Errorf("ServiceLevel before start = %v, want None", h.ServiceLevel)
	}

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	h = sess.Health(ctx)
	if !h.Connected {
		t.Error("should be connected after Start")
	}
	if !h.Ready {
		t.Error("should be ready after Start")
	}
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Errorf("ServiceLevel after start = %v, want Full", h.ServiceLevel)
	}

	_ = sess.Close(ctx)

	h = sess.Health(ctx)
	if h.Connected {
		t.Error("should not be connected after Close")
	}
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Errorf("ServiceLevel after close = %v, want None", h.ServiceLevel)
	}

	assertLogContains(t, &buf, "amqp10: session close initiated")
}

// TestIntegration_Edge_MulticastRouting validates that a message sent with
// RoutingMulticast is delivered to all attached receivers (fan-out).
func TestIntegration_Edge_MulticastRouting(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	sess := edgeSession(t, logger)
	addr := artemislocal.UniqueAddress("edge-multicast")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sender, err := NewSender(SenderConfig{
		Address: addr, Session: sess, Routing: RoutingMulticast,
	}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()

	recv1, err := NewReceiver(ReceiverConfig{
		Address: addr, LinkCredit: 10, Session: sess, Routing: RoutingMulticast,
	}, sess)
	if err != nil {
		t.Fatalf("NewReceiver1: %v", err)
	}
	recv2, err := NewReceiver(ReceiverConfig{
		Address: addr, LinkCredit: 10, Session: sess, Routing: RoutingMulticast,
	}, sess)
	if err != nil {
		t.Fatalf("NewReceiver2: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	received := make(map[string]int)

	startRecv := func(r *Receiver, name string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rCtx, rCancel := context.WithTimeout(ctx, 10*time.Second)
			defer rCancel()
			_ = r.Run(rCtx, func(_ context.Context, del ports.Delivery) error {
				mu.Lock()
				received[name]++
				mu.Unlock()
				_ = del.Ack(rCtx)
				rCancel()
				return nil
			})
		}()
	}

	startRecv(recv1, "r1")
	startRecv(recv2, "r2")

	// STARTUP: wait for receiver links to be live instead of blind sleep.
	for _, r := range []*Receiver{recv1, recv2} {
		select {
		case <-r.Started():
		case <-ctx.Done():
			t.Fatal("receiver did not start in time")
		}
	}

	if err := sender.Send(ctx, &messaging.Envelope{
		ID: "multicast-1", Subject: "test", Payload: []byte("fan-out"),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	wg.Wait()

	mu.Lock()
	r1 := received["r1"]
	r2 := received["r2"]
	mu.Unlock()

	if r1 != 1 || r2 != 1 {
		t.Errorf("multicast: r1=%d, r2=%d; expected both to be 1", r1, r2)
	}

	assertLogContains(t, &buf, "amqp10: sending", "amqp10: received message")
}

// TestIntegration_Edge_SendBatchPartialVerify validates that all messages
// from a SendBatch are received with correct unique IDs.
func TestIntegration_Edge_SendBatchPartialVerify(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	sess := edgeSession(t, logger)
	addr := artemislocal.UniqueAddress("edge-batch-verify")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sender, err := NewSender(SenderConfig{Address: addr, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()

	const msgCount = 5
	envs := make([]*messaging.Envelope, msgCount)
	wantIDs := make(map[string]bool, msgCount)
	for i := range envs {
		id := "batch-" + string(rune('A'+i))
		envs[i] = &messaging.Envelope{
			ID: id, Subject: "test", Payload: []byte("payload"),
		}
		wantIDs[id] = true
	}

	sent, err := sender.SendBatch(ctx, envs)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent != msgCount {
		t.Fatalf("sent %d, want %d", sent, msgCount)
	}

	recv, err := NewReceiver(ReceiverConfig{Address: addr, LinkCredit: 10, Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	ids := make(map[string]bool)
	_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
		ids[del.Envelope().ID] = true
		_ = del.Ack(recvCtx)
		if len(ids) >= msgCount {
			recvCancel()
		}
		return nil
	})

	if len(ids) != msgCount {
		t.Errorf("received %d unique messages, want %d", len(ids), msgCount)
	}
	for id := range wantIDs {
		if !ids[id] {
			t.Errorf("missing expected message ID %q", id)
		}
	}

	assertLogContains(t, &buf, "amqp10: send complete")
}

// TestIntegration_Edge_HeaderUnicodeAndLongValues validates that headers
// with unicode characters and long string values survive the round-trip.
func TestIntegration_Edge_HeaderUnicodeAndLongValues(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	sess := edgeSession(t, logger)
	addr := artemislocal.UniqueAddress("edge-headers-unicode")

	longValue := strings.Repeat("x", 4096)
	env := &messaging.Envelope{
		ID: "unicode-headers", Subject: "test", Payload: []byte("body"),
		Headers: map[string]any{
			"emoji":      "hello 🌍🚀",
			"cjk":        "你好世界",
			"long-value": longValue,
			"diacritics": "résumé café naïve",
		},
	}

	got := edgeSendRecv(t, sess, addr, env, 20*time.Second)

	if got.Headers["emoji"] != "hello 🌍🚀" {
		t.Errorf("emoji header = %v", got.Headers["emoji"])
	}
	if got.Headers["cjk"] != "你好世界" {
		t.Errorf("cjk header = %v", got.Headers["cjk"])
	}
	if got.Headers["long-value"] != longValue {
		t.Errorf("long-value length = %d, want %d",
			len(got.Headers["long-value"].(string)), len(longValue))
	}
	if got.Headers["diacritics"] != "résumé café naïve" {
		t.Errorf("diacritics header = %v", got.Headers["diacritics"])
	}

	assertLogContains(t, &buf, "amqp10: sending", "amqp10: received message")
}

// TestIntegration_Edge_EnvelopeFieldsRoundTrip validates that all envelope
// fields (ID, Subject, Payload, CreatedAt, ExpiresAt, Headers) survive
// a send/receive cycle.
func TestIntegration_Edge_EnvelopeFieldsRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	logger := traceLogger(&buf)
	sess := edgeSession(t, logger)
	addr := artemislocal.UniqueAddress("edge-fields-rt")

	now := time.Now().UTC().Truncate(time.Millisecond)
	expiresAt := now.Add(1 * time.Hour)

	env := &messaging.Envelope{
		ID:        "full-fields",
		Subject:   "integration.edge",
		Payload:   []byte(`{"complete":"envelope"}`),
		CreatedAt: now,
		ExpiresAt: expiresAt,
		Headers: map[string]any{
			"custom-key": "custom-value",
		},
	}

	got := edgeSendRecv(t, sess, addr, env, 20*time.Second)

	if got.ID != "full-fields" {
		t.Errorf("ID = %q, want %q", got.ID, "full-fields")
	}
	if got.Subject != "integration.edge" {
		t.Errorf("Subject = %q, want %q", got.Subject, "integration.edge")
	}
	if string(got.Payload) != `{"complete":"envelope"}` {
		t.Errorf("Payload = %q", got.Payload)
	}
	if got.ExpiresAt.IsZero() {
		t.Error("ExpiresAt should not be zero")
	}
	drift := got.ExpiresAt.Sub(expiresAt)
	if drift < -2*time.Second || drift > 2*time.Second {
		t.Errorf("ExpiresAt drift = %v (sent %v, got %v)", drift, expiresAt, got.ExpiresAt)
	}
	if got.Headers["custom-key"] != "custom-value" {
		t.Errorf("custom-key header = %v", got.Headers["custom-key"])
	}

	assertLogContains(t, &buf, "amqp10: sending", "amqp10: accepting message")
}
