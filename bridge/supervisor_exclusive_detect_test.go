package bridge

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
)

// exclRxConfig is a minimal PluginConfig carrying just the exclusive flag,
// so detectSwapMode's config-driven detection can be exercised in isolation.
type exclRxConfig struct{ excl bool }

func (c *exclRxConfig) Kind() string    { return "test.exclrx" }
func (c *exclRxConfig) Validate() error { return nil }

// configExclusiveTransportFactory implements the optional
// exclusiveIdentityConfigDetector hook but deliberately does NOT advertise
// CapExclusiveIdentity. That isolates detectSwapMode's config-driven path
// (the first reconfig that INTRODUCES an exclusive consumer) from the
// post-build capability latch, which is what the amqp091 factory relies on.
type configExclusiveTransportFactory struct {
	fakeTransportFactory
}

func (f *configExclusiveTransportFactory) ConfigRequiresExclusiveIdentity(cfg ports.PluginConfig) bool {
	ec, ok := cfg.(*exclRxConfig)
	return ok && ec.excl
}

// TestDetectSwapMode_IntroduceExclusiveViaReceiverConfig covers the gap the
// capability latch alone leaves open: a reconfig that introduces an exclusive
// consumer for the first time must still pick the serialized swap, detected
// from the incoming receiver config via the optional factory hook.
func TestDetectSwapMode_IntroduceExclusiveViaReceiverConfig(t *testing.T) {
	newSup := func() *Supervisor {
		s := NewSupervisor()
		s.RegisterTransport("cfgexcl", &configExclusiveTransportFactory{})
		s.RegisterTransport("fake", &fakeTransportFactory{})
		return s
	}

	t.Run("ExclusiveReceiverConfigSelectsPrepareCommit", func(t *testing.T) {
		s := newSup()
		cfg := &ports.BridgeConfig{
			Receivers: []ports.ReceiverDef{
				{ID: "rx", Transport: "cfgexcl", Config: &exclRxConfig{excl: true}},
			},
		}
		assert.Equal(t, SwapPrepareCommit, s.detectSwapMode(cfg))
	})

	t.Run("NonExclusiveReceiverConfigStaysOverlap", func(t *testing.T) {
		s := newSup()
		cfg := &ports.BridgeConfig{
			Receivers: []ports.ReceiverDef{
				{ID: "rx", Transport: "cfgexcl", Config: &exclRxConfig{excl: false}},
			},
		}
		assert.Equal(t, SwapOverlap, s.detectSwapMode(cfg))
	})

	t.Run("TransportInheritedFromSession", func(t *testing.T) {
		s := newSup()
		cfg := &ports.BridgeConfig{
			Sessions: []ports.SessionDef{
				{ID: "s1", Transport: "cfgexcl"},
			},
			Receivers: []ports.ReceiverDef{
				// Empty Transport → resolved from session s1 (cfgexcl).
				{ID: "rx", SessionID: "s1", Config: &exclRxConfig{excl: true}},
			},
		}
		assert.Equal(t, SwapPrepareCommit, s.detectSwapMode(cfg))
	})

	t.Run("FactoryWithoutHookStaysOverlap", func(t *testing.T) {
		s := newSup()
		cfg := &ports.BridgeConfig{
			Receivers: []ports.ReceiverDef{
				{ID: "rx", Transport: "fake", Config: &exclRxConfig{excl: true}},
			},
		}
		assert.Equal(t, SwapOverlap, s.detectSwapMode(cfg))
	})
}

// TestDetectSwapMode_ConfigDeclaredExclusiveOnSilentTransport covers HIGH-1: a
// config that DECLARES an exclusive session must pick the serialized
// PrepareCommit swap even when its transport is capability-silent (models
// amqp10, which obeys the single-use exclusive rule but never advertises
// CapExclusiveIdentity). Under SwapOverlap the new exclusive session would be
// opened while the old one is still active, colliding on the broker identity.
func TestDetectSwapMode_ConfigDeclaredExclusiveOnSilentTransport(t *testing.T) {
	// "fake" is the capability-silent factory (no CapExclusiveIdentity, no
	// exclusiveIdentityConfigDetector hook) — so ONLY the config-declared path
	// can select PrepareCommit here.
	newSup := func() *Supervisor {
		s := NewSupervisor()
		s.RegisterTransport("fake", &fakeTransportFactory{})
		return s
	}

	t.Run("NamedSessionModeExclusiveSelectsPrepareCommit", func(t *testing.T) {
		s := newSup()
		cfg := &ports.BridgeConfig{
			Sessions: []ports.SessionDef{
				{ID: "s1", Transport: "fake", SessionMode: "exclusive"},
			},
		}
		assert.Equal(t, SwapPrepareCommit, s.detectSwapMode(cfg))
	})

	t.Run("InlineRouteSessionSelectsPrepareCommit", func(t *testing.T) {
		s := newSup()
		cfg := &ports.BridgeConfig{
			Sessions: []ports.SessionDef{
				{ID: "s1", Transport: "fake"},
			},
			Routes: []ports.RouteDef{
				// A non-nil inline session is ALWAYS exclusive (ports/blueprint.go:359).
				{ID: "r1", Session: &ports.RouteSessionDef{SessionID: "s1", SenderID: "snd1"}},
			},
		}
		assert.Equal(t, SwapPrepareCommit, s.detectSwapMode(cfg))
	})

	t.Run("SharedSessionOnSilentTransportStaysOverlap", func(t *testing.T) {
		s := newSup()
		cfg := &ports.BridgeConfig{
			Sessions: []ports.SessionDef{
				// No session_mode and no inline route session → not exclusive.
				{ID: "s1", Transport: "fake", SessionMode: "shared"},
			},
		}
		assert.Equal(t, SwapOverlap, s.detectSwapMode(cfg))
	})
}
