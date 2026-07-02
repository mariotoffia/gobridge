// ═══════════════════════════════════════════════
// Production-readiness remediation tests: config & factory guards.
//
// Covers:
//   - auto_ack rejection for managed routes (no silent message loss).
//   - immediate rejection (unsupported by RabbitMQ; closes the channel).
//   - prefetch-count default applied on the typed config path.
//   - vhost plumbed into the SDK dial config.
//
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestConfig_Validate_RejectsAutoAck verifies that a managed receiver
// config with auto_ack=true is rejected at parse/validate time, so the
// bridge never broker-acks on delivery and loses messages on a
// downstream failure.
func TestConfig_Validate_RejectsAutoAck(t *testing.T) {
	c := Config{Receiver: ReceiverParams{QueueName: "q", AutoAck: true}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected Validate to reject auto_ack=true")
	}

	// Manual settlement (the default) must validate.
	ok := Config{Receiver: ReceiverParams{QueueName: "q"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("manual-ack receiver should validate, got %v", err)
	}
}

// TestConfig_Validate_RejectsImmediate verifies immediate=true is
// rejected: RabbitMQ removed basic.publish immediate in 3.0 and closes
// the channel when it is set.
func TestConfig_Validate_RejectsImmediate(t *testing.T) {
	c := Config{Sender: SenderParams{Exchange: "x", Immediate: true}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected Validate to reject immediate=true")
	}

	ok := Config{Sender: SenderParams{Exchange: "x"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("non-immediate sender should validate, got %v", err)
	}
}

// TestReceiverParams_ApplyDefaults_Prefetch verifies a zero prefetch on
// the typed config path is replaced by the bounded default rather than
// left as 0 (which would mean unbounded broker push).
func TestReceiverParams_ApplyDefaults_Prefetch(t *testing.T) {
	var p ReceiverParams
	p.applyDefaults()
	if p.PrefetchCount != defaultPrefetchCount {
		t.Fatalf("PrefetchCount = %d, want default %d", p.PrefetchCount, defaultPrefetchCount)
	}

	explicit := ReceiverParams{PrefetchCount: 3}
	explicit.applyDefaults()
	if explicit.PrefetchCount != 3 {
		t.Fatalf("explicit PrefetchCount must be preserved, got %d", explicit.PrefetchCount)
	}
}

// TestReceiverFactory_NewReceiver_RejectsAutoAck verifies the managed
// factory re-rejects auto_ack even when a programmatic spec bypassed the
// config decoder's Validate.
func TestReceiverFactory_NewReceiver_RejectsAutoAck(t *testing.T) {
	rf := NewReceiverFactory(slog.Default())
	sess := NewSession(SessionOptions{BrokerURL: "amqp://localhost/"}, connectivity.SessionEphemeral, slog.Default())
	defer func() { _ = sess.Close(context.Background()) }()

	_, err := rf.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "r-autoack",
		Config: Config{Receiver: ReceiverParams{QueueName: "q", AutoAck: true}},
	}, sess)
	if err == nil {
		t.Fatal("expected factory to reject auto_ack=true")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
}

// TestReceiverFactory_NewReceiver_AppliesPrefetchDefault verifies the
// typed config path no longer loses the prefetch default (regression for
// the struct-zero == unbounded footgun).
func TestReceiverFactory_NewReceiver_AppliesPrefetchDefault(t *testing.T) {
	rf := NewReceiverFactory(slog.Default())
	sess := NewSession(SessionOptions{BrokerURL: "amqp://localhost/"}, connectivity.SessionEphemeral, slog.Default())
	defer func() { _ = sess.Close(context.Background()) }()

	r, err := rf.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "r-prefetch",
		Config: Config{Receiver: ReceiverParams{QueueName: "q"}}, // no prefetch_count
	}, sess)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	recv := r.(*Receiver)
	if recv.cfg.PrefetchCount != defaultPrefetchCount {
		t.Fatalf("PrefetchCount = %d, want default %d", recv.cfg.PrefetchCount, defaultPrefetchCount)
	}
}

// TestSenderFactory_NewSender_RejectsImmediate verifies the managed
// factory re-rejects immediate even when a programmatic spec bypassed
// Config.Validate.
func TestSenderFactory_NewSender_RejectsImmediate(t *testing.T) {
	sf := NewSenderFactory(slog.Default())
	sess := NewSession(SessionOptions{BrokerURL: "amqp://localhost/"}, connectivity.SessionEphemeral, slog.Default())
	defer func() { _ = sess.Close(context.Background()) }()

	_, err := sf.NewSender(context.Background(), ports.SenderSpec{
		ID:     "s-immediate",
		Config: Config{Sender: SenderParams{Exchange: "x", Immediate: true}},
	}, sess)
	if err == nil {
		t.Fatal("expected factory to reject immediate=true")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
}

// TestDialConfig_Vhost verifies the configured vhost is placed into the
// SDK dial config (DialConfig only falls back to the URL path when
// Config.Vhost is empty), and that an empty vhost stays empty so the URL
// path keeps working (backwards-compatible).
func TestDialConfig_Vhost(t *testing.T) {
	withVhost := dialConfig(SessionOptions{Vhost: "/tenant-a", Heartbeat: 0})
	if withVhost.Vhost != "/tenant-a" {
		t.Fatalf("Vhost = %q, want %q", withVhost.Vhost, "/tenant-a")
	}

	empty := dialConfig(SessionOptions{})
	if empty.Vhost != "" {
		t.Fatalf("empty Vhost should stay empty for URL fallback, got %q", empty.Vhost)
	}

	// Heartbeat is also threaded through from options into the dial config.
	withHeartbeat := dialConfig(SessionOptions{Heartbeat: 30 * time.Second})
	if withHeartbeat.Heartbeat != 30*time.Second {
		t.Fatalf("Heartbeat = %v, want %v", withHeartbeat.Heartbeat, 30*time.Second)
	}
}
