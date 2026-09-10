// ═══════════════════════════════════════════════
// Production-readiness remediation tests: exclusive-identity capability.
//
// Covers the MEDIUM finding — amqp091 did not advertise CapExclusiveIdentity,
// so the supervisor picked the Overlap swap mode on reconfig and ran the old
// and new instances concurrently. Exclusive consumers then race a 403
// ACCESS_REFUSED against the reconnect budget and can trip terminal teardown.
// The factory now latches CapExclusiveIdentity once it has built an exclusive
// receiver, so the supervisor serializes (PrepareCommit) old/new instances.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// TestFactory_Capabilities_AdvertisesExclusiveIdentity proves the factory
// starts without CapExclusiveIdentity and latches it once an exclusive
// receiver is built.
func TestFactory_Capabilities_AdvertisesExclusiveIdentity(t *testing.T) {
	f := NewFactory(slog.Default())

	if slices.Contains(f.Capabilities(), ports.CapExclusiveIdentity) {
		t.Fatal("factory should not advertise CapExclusiveIdentity before any exclusive receiver is built")
	}

	sess := NewSession(SessionOptions{BrokerURL: "amqp://localhost/"}, connectivity.SessionEphemeral, slog.Default())
	defer func() { _ = sess.Close(context.Background()) }()

	// A non-exclusive receiver must NOT flip the advertisement.
	if _, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "shared",
		Config: Config{Receiver: ReceiverParams{QueueName: "shared-q"}},
	}, sess); err != nil {
		t.Fatalf("NewReceiver(non-exclusive): %v", err)
	}
	if slices.Contains(f.Capabilities(), ports.CapExclusiveIdentity) {
		t.Fatal("a non-exclusive receiver must not advertise CapExclusiveIdentity")
	}

	// Building an exclusive receiver latches the advertisement.
	if _, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "excl",
		Config: Config{Receiver: ReceiverParams{QueueName: "excl-q", Exclusive: true}},
	}, sess); err != nil {
		t.Fatalf("NewReceiver(exclusive): %v", err)
	}
	if !slices.Contains(f.Capabilities(), ports.CapExclusiveIdentity) {
		t.Fatal("factory must advertise CapExclusiveIdentity after building an exclusive receiver")
	}

	// The other, always-present capabilities are still advertised.
	for _, want := range []ports.Capability{
		ports.CapStatefulSession, ports.CapSourceRedelivery, ports.CapPlanDrivenSubscriptions,
	} {
		if !slices.Contains(f.Capabilities(), want) {
			t.Fatalf("capability %q missing from advertisement", want)
		}
	}
}

// otherPluginConfig is a foreign PluginConfig type that configFromSpec cannot
// decode, modelling a garbage / wrong-transport config handed to the factory.
type otherPluginConfig struct{}

func (otherPluginConfig) Kind() string    { return "other" }
func (otherPluginConfig) Validate() error { return nil }

// TestFactory_ConfigRequiresExclusiveIdentity covers a config-shape rule: the
// supervisor must be able to detect an exclusivity-introducing reconfig from
// the RECEIVER config BEFORE any receiver (and thus the exclusiveSeen latch)
// exists. A config with exclusive=true reports true; a non-exclusive config,
// nil, or an undecodable config all report false (no false positive). The
// query must be pure — it must NOT latch the advertisement.
func TestFactory_ConfigRequiresExclusiveIdentity(t *testing.T) {
	f := NewFactory(slog.Default())

	if !f.ConfigRequiresExclusiveIdentity(Config{
		Receiver: ReceiverParams{QueueName: "q", Exclusive: true},
	}) {
		t.Fatal("exclusive receiver config must require exclusive identity")
	}
	if !f.ConfigRequiresExclusiveIdentity(&Config{
		Receiver: ReceiverParams{QueueName: "q", Exclusive: true},
	}) {
		t.Fatal("exclusive receiver config (pointer) must require exclusive identity")
	}
	if f.ConfigRequiresExclusiveIdentity(Config{
		Receiver: ReceiverParams{QueueName: "q"},
	}) {
		t.Fatal("non-exclusive receiver config must not require exclusive identity")
	}
	if f.ConfigRequiresExclusiveIdentity(nil) {
		t.Fatal("nil config must return false")
	}
	if f.ConfigRequiresExclusiveIdentity(otherPluginConfig{}) {
		t.Fatal("undecodable config must return false")
	}

	// Pure query: inspecting an exclusive config must NOT latch the
	// steady-state advertisement (that is NewReceiver's job).
	if slices.Contains(f.Capabilities(), ports.CapExclusiveIdentity) {
		t.Fatal("ConfigRequiresExclusiveIdentity must not latch CapExclusiveIdentity")
	}
}
