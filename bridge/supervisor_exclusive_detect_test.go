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

// TestDetectSwapMode_ConfigDeclaredExclusiveOnSilentTransport covers that a
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

// TestDetectSwapMode_LeavingExclusiveSerializes covers the reverse transition:
// the running config holds an exclusive identity and the incoming one does
// not. Nothing in the incoming config claims exclusivity, yet an overlapping
// swap starts the new consumer while the old exclusive one is still attached,
// and the broker refuses it. The running config has to count. A factory latch
// cannot stand in for it: a composition root that builds fresh factories for
// every swap never sees one set.
func TestDetectSwapMode_LeavingExclusiveSerializes(t *testing.T) {
	// Capability-silent, hook-only factory: the running config is the sole
	// source of exclusivity in every case below. The same instance is also
	// registered under an alias, the way amqp091 and amqp.amqp091 share one.
	newSup := func(running *ports.BridgeConfig) *Supervisor {
		s := NewSupervisor()
		excl := &configExclusiveTransportFactory{}
		s.RegisterTransport("cfgexcl", excl)
		s.RegisterTransport("cfgexcl.alias", excl)
		s.RegisterTransport("fake", &fakeTransportFactory{})
		s.cfg = running
		return s
	}
	receiver := func(excl bool) *ports.BridgeConfig {
		return &ports.BridgeConfig{Receivers: []ports.ReceiverDef{
			{ID: "rx", Transport: "cfgexcl", Config: &exclRxConfig{excl: excl}},
		}}
	}
	session := func(mode string) *ports.BridgeConfig {
		return &ports.BridgeConfig{Sessions: []ports.SessionDef{
			{ID: "s1", Transport: "cfgexcl", SessionMode: mode},
		}}
	}

	t.Run("ExclusiveReceiverToNonExclusiveSelectsPrepareCommit", func(t *testing.T) {
		assert.Equal(t, SwapPrepareCommit, newSup(receiver(true)).detectSwapMode(receiver(false)))
	})

	t.Run("DeclaredExclusiveSessionToSharedSelectsPrepareCommit", func(t *testing.T) {
		assert.Equal(t, SwapPrepareCommit, newSup(session("exclusive")).detectSwapMode(session("shared")))
	})

	t.Run("NonExclusiveOnBothSidesStaysOverlap", func(t *testing.T) {
		assert.Equal(t, SwapOverlap, newSup(receiver(false)).detectSwapMode(receiver(false)))
	})

	t.Run("DroppingTheTransportAltogetherStaysOverlap", func(t *testing.T) {
		// Nothing in the new runtime can contend for an identity held on a
		// transport it no longer uses, so the zero-downtime swap stays.
		next := &ports.BridgeConfig{Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "fake"}}}
		assert.Equal(t, SwapOverlap, newSup(receiver(true)).detectSwapMode(next))
	})

	t.Run("AliasOfTheSameFactoryIsTheSameTransport", func(t *testing.T) {
		next := &ports.BridgeConfig{Receivers: []ports.ReceiverDef{
			{ID: "rx", Transport: "cfgexcl.alias", Config: &exclRxConfig{excl: false}},
		}}
		assert.Equal(t, SwapPrepareCommit, newSup(receiver(true)).detectSwapMode(next))
	})

	t.Run("SenderAloneKeepsTheTransportInUse", func(t *testing.T) {
		next := &ports.BridgeConfig{Senders: []ports.SenderDef{{ID: "tx", Transport: "cfgexcl"}}}
		assert.Equal(t, SwapPrepareCommit, newSup(session("exclusive")).detectSwapMode(next))
	})

	t.Run("FirstApplyHasNoRunningConfig", func(t *testing.T) {
		assert.Equal(t, SwapOverlap, newSup(nil).detectSwapMode(receiver(false)))
	})
}
