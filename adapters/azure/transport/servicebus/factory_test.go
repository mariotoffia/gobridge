package servicebus

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// verifies NewSession returns a nil session with no error for stateless Service Bus transport.
func TestFactory_NewSession_ReturnsNilNil(t *testing.T) {
	f := NewFactory(nil)

	sess, err := f.NewSession(context.Background(), ports.SessionSpec{
		ID:        "asb-session",
		Transport: "servicebus",
	})

	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if sess != nil {
		t.Fatal("NewSession should return nil session for stateless servicebus transport")
	}
}

// verifies Factory declares the PeekLock source capabilities (parity
// with SQS): visibility extension, source redelivery, delayed send.
func TestFactory_Capabilities(t *testing.T) {
	f := NewFactory(nil)
	caps := f.Capabilities()

	want := []ports.Capability{
		ports.CapVisibilityExtension,
		ports.CapSourceRedelivery,
		ports.CapDelayedSend,
	}
	if len(caps) != len(want) {
		t.Fatalf("expected %d capabilities, got %d: %v", len(want), len(caps), caps)
	}
	for _, w := range want {
		if !slices.Contains(caps, w) {
			t.Errorf("Factory.Capabilities() missing %q; got %v", w, caps)
		}
	}
}

// a ReceiveAndDelete route must NOT advertise CapVisibilityExtension
// (Extend is a no-op) nor CapSourceRedelivery (no redelivery) — otherwise
// the validator's "no retry + no DLQ = silent drop" check is masked. A
// PeekLock route advertises the full set.
func TestConfig_Capabilities_ModeAware(t *testing.T) {
	peek := Config{Receiver: ReceiverParams{QueueName: "q"}}.Capabilities()
	for _, w := range []ports.Capability{
		ports.CapVisibilityExtension,
		ports.CapSourceRedelivery,
		ports.CapDelayedSend,
	} {
		if !slices.Contains(peek, w) {
			t.Errorf("PeekLock Config.Capabilities() missing %q; got %v", w, peek)
		}
	}

	// a PeekLock topic subscription (no QueueName) cannot honour a
	// delayed Retry — scheduling would fan out to sibling subscriptions,
	// so it falls back to an immediate Abandon and CapDelayedSend is
	// withheld. CapVisibilityExtension/CapSourceRedelivery REMAIN, so the
	// route still clears the validator's "no retry + no DLQ" gate.
	sub := Config{Receiver: ReceiverParams{TopicName: "t", SubscriptionName: "s"}}.Capabilities()
	if slices.Contains(sub, ports.CapDelayedSend) {
		t.Errorf("subscription Config.Capabilities() must NOT advertise CapDelayedSend; got %v", sub)
	}
	for _, w := range []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery} {
		if !slices.Contains(sub, w) {
			t.Errorf("subscription Config.Capabilities() missing %q; got %v", w, sub)
		}
	}

	// receive_mode matched case-insensitively, mirroring receiveAndDelete().
	for _, mode := range []string{"ReceiveAndDelete", "receiveanddelete"} {
		rad := Config{Receiver: ReceiverParams{QueueName: "q", ReceiveMode: mode}}.Capabilities()
		if slices.Contains(rad, ports.CapVisibilityExtension) {
			t.Errorf("ReceiveAndDelete (%q) must not advertise CapVisibilityExtension; got %v", mode, rad)
		}
		if slices.Contains(rad, ports.CapSourceRedelivery) {
			t.Errorf("ReceiveAndDelete (%q) must not advertise CapSourceRedelivery; got %v", mode, rad)
		}
	}
}

// verifies Config.EffectiveVisibilityTimeout reports the per-route lock
// duration (the ASB visibility analog), falling back to the 30s default
// when lock_duration is unset. The builder threads this value into the
// runtime validator in preference to Factory.VisibilityTimeout(), so a
// route with a short lock_duration is correctly guarded (Finding 2 /).
func TestConfig_EffectiveVisibilityTimeout(t *testing.T) {
	if got := (Config{}).EffectiveVisibilityTimeout(); got != 30*time.Second {
		t.Errorf("unset lock_duration: got %v, want 30s", got)
	}
	cfg := Config{Receiver: ReceiverParams{LockDuration: 10 * time.Second}}
	if got := cfg.EffectiveVisibilityTimeout(); got != 10*time.Second {
		t.Errorf("configured lock_duration: got %v, want 10s", got)
	}
}

// verifies Config.AutoExtendEnabled defaults on (nil) and honours an
// explicit flag, matching ReceiverConfig.autoExtendEnabled. The validator
// uses it to skip the SendTimeout-vs-window check for renewed windows.
func TestConfig_AutoExtendEnabled(t *testing.T) {
	if !(Config{}).AutoExtendEnabled() {
		t.Error("unset auto_extend should default to enabled")
	}
	off := false
	if (Config{Receiver: ReceiverParams{AutoExtend: &off}}).AutoExtendEnabled() {
		t.Error("auto_extend=false should report disabled")
	}
	on := true
	if !(Config{Receiver: ReceiverParams{AutoExtend: &on}}).AutoExtendEnabled() {
		t.Error("auto_extend=true should report enabled")
	}
}

// verifies Factory.NewReceiver builds a queue receiver from spec options.
func TestFactory_NewReceiver(t *testing.T) {
	f := NewFactory(nil)

	spec := ports.ReceiverSpec{
		ID: "recv-1",
		Config: Config{
			Receiver:   ReceiverParams{QueueName: "test-queue"},
			Connection: ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake")},
		},
	}

	recv, err := f.NewReceiver(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewReceiver returned error: %v", err)
	}
	if recv == nil {
		t.Fatal("NewReceiver returned nil receiver")
	}
}

// verifies Factory.NewSender builds a queue sender from spec options.
func TestFactory_NewSender(t *testing.T) {
	f := NewFactory(nil)

	spec := ports.SenderSpec{
		ID: "send-1",
		Config: Config{
			Sender:     SenderParams{QueueName: "test-queue"},
			Connection: ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake")},
		},
	}

	snd, err := f.NewSender(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewSender returned error: %v", err)
	}
	if snd == nil {
		t.Fatal("NewSender returned nil sender")
	}
}

// verifies Factory.NewReceiver builds a topic subscription receiver from options.
func TestFactory_NewReceiver_TopicSubscription(t *testing.T) {
	f := NewFactory(nil)

	spec := ports.ReceiverSpec{
		ID: "recv-topic",
		Config: Config{
			Receiver:   ReceiverParams{TopicName: "test-topic", SubscriptionName: "test-sub"},
			Connection: ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake")},
		},
	}

	recv, err := f.NewReceiver(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewReceiver returned error: %v", err)
	}
	if recv == nil {
		t.Fatal("NewReceiver returned nil receiver")
	}
}

// verifies Factory.NewSender builds a topic sender from options.
func TestFactory_NewSender_Topic(t *testing.T) {
	f := NewFactory(nil)

	spec := ports.SenderSpec{
		ID: "send-topic",
		Config: Config{
			Sender:     SenderParams{TopicName: "test-topic"},
			Connection: ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake")},
		},
	}

	snd, err := f.NewSender(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewSender returned error: %v", err)
	}
	if snd == nil {
		t.Fatal("NewSender returned nil sender")
	}
}

// verifies ReceiverFactory.NewReceiver from a ports.ReceiverSpec.
func TestReceiverFactory_NewReceiver(t *testing.T) {
	rf := NewReceiverFactory(nil)

	spec := ports.ReceiverSpec{
		ID: "recv-direct",
		Config: Config{
			Receiver:   ReceiverParams{QueueName: "direct-queue"},
			Connection: ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake")},
		},
	}

	recv, err := rf.NewReceiver(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewReceiver returned error: %v", err)
	}
	if recv == nil {
		t.Fatal("NewReceiver returned nil receiver")
	}
}

// verifies SenderFactory.NewSender from a ports.SenderSpec.
func TestSenderFactory_NewSender(t *testing.T) {
	sf := NewSenderFactory(nil)

	spec := ports.SenderSpec{
		ID: "send-direct",
		Config: Config{
			Sender:     SenderParams{QueueName: "direct-queue"},
			Connection: ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=fake")},
		},
	}

	snd, err := sf.NewSender(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewSender returned error: %v", err)
	}
	if snd == nil {
		t.Fatal("NewSender returned nil sender")
	}
}
