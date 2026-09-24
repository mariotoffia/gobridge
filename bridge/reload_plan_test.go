package bridge

import (
	"math"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

// httpEndpointTransportFactory advertises what the HTTP transport's factory
// does: its endpoints mount on a shared mux.
type httpEndpointTransportFactory struct{ fakeTransportFactory }

func (f *httpEndpointTransportFactory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapHTTPEndpoint}
}

var _ ports.TransportFactory = (*httpEndpointTransportFactory)(nil)

func reloadTestTransports() map[string]ports.TransportFactory {
	return map[string]ports.TransportFactory{
		"fake":      &fakeTransportFactory{},
		"exclusive": &exclusiveTransportFactory{},
		"http":      &httpEndpointTransportFactory{},
	}
}

func mustPlanInPlace(t *testing.T, running, next *ports.BridgeConfig) *InPlaceReload {
	t.Helper()
	plan, ok := PlanInPlaceReload(running, next, reloadTestTransports())
	require.True(t, ok, "expected the reload to be eligible in place")
	require.NotNil(t, plan)
	return plan
}

func requireNotInPlace(t *testing.T, running, next *ports.BridgeConfig) {
	t.Helper()
	plan, ok := PlanInPlaceReload(running, next, reloadTestTransports())
	require.False(t, ok, "expected the reload to need a full replacement")
	require.Nil(t, plan)
}

func TestPlanInPlaceReload_OneOwnerChangedRetiresAndAddsOnlyThatUnit(t *testing.T) {
	running := reloadTestConfig("a", "b")
	next := reloadTestConfig("a", "b")
	next.Bindings[0].Address = "topic/a-v2"

	plan := mustPlanInPlace(t, running, next)

	require.Len(t, plan.retire, 1)
	require.Len(t, plan.add, 1)
	require.Equal(t, []string{"a"}, plan.retire[0].routes)
	require.Equal(t, []string{"a"}, plan.add[0].routes)
	require.Equal(t, "topic/a", plan.retire[0].sub.Bindings[0].Address)
	require.Equal(t, "topic/a-v2", plan.add[0].sub.Bindings[0].Address)
	require.Same(t, running, plan.running)
	require.Same(t, next, plan.next)
}

func TestPlanInPlaceReload_UnchangedConfigPlansNothing(t *testing.T) {
	next := reloadTestConfig("b", "a")
	next.Version = 9

	plan := mustPlanInPlace(t, reloadTestConfig("a", "b"), next)

	require.Empty(t, plan.retire)
	require.Empty(t, plan.add)
	require.False(t, plan.Serialized())
}

func TestPlanInPlaceReload_BridgeSettingsChangeIsNotEligible(t *testing.T) {
	next := reloadTestConfig("a", "b")
	next.Bridge.DrainTimeout = "2s"

	requireNotInPlace(t, reloadTestConfig("a", "b"), next)
}

func TestPlanInPlaceReload_StoresChangeIsNotEligible(t *testing.T) {
	next := reloadTestConfig("a", "b")
	next.Stores.DLQ = &ports.StoreConfig{Type: "memory"}

	requireNotInPlace(t, reloadTestConfig("a", "b"), next)
}

func TestPlanInPlaceReload_HTTPEndpointUnitIsNotEligible(t *testing.T) {
	withHTTPOwner := reloadTestConfig("a")
	addReloadTestOwner(withHTTPOwner, "h", "http")

	// An added unit and a retired unit on an HTTP endpoint both need the full
	// replacement: the shared mux can neither mount a path twice nor unmount it.
	requireNotInPlace(t, reloadTestConfig("a"), withHTTPOwner)
	requireNotInPlace(t, withHTTPOwner, reloadTestConfig("a"))

	// The same owner on an ordinary transport reloads in place.
	withFakeOwner := reloadTestConfig("a", "h")
	plan := mustPlanInPlace(t, reloadTestConfig("a"), withFakeOwner)
	require.Len(t, plan.add, 1)
}

// A transport the caller registered no factory for has capabilities nobody can
// read, so it may mount an HTTP endpoint: the reload is not planned in place.
func TestPlanInPlaceReload_UnknownTransportIsNotEligible(t *testing.T) {
	withUnknownOwner := reloadTestConfig("a")
	addReloadTestOwner(withUnknownOwner, "u", "unregistered")

	requireNotInPlace(t, reloadTestConfig("a"), withUnknownOwner)
	requireNotInPlace(t, withUnknownOwner, reloadTestConfig("a"))
}

func TestPlanInPlaceReload_UnchangedHTTPUnitStaysEligible(t *testing.T) {
	running := reloadTestConfig("a")
	addReloadTestOwner(running, "h", "http")
	next := reloadTestConfig("a")
	addReloadTestOwner(next, "h", "http")
	next.Bindings[0].Address = "topic/a-v2"

	plan := mustPlanInPlace(t, running, next)

	retiredRoutes, addedRoutes, _, _ := plan.Summary()
	require.Equal(t, []string{"a"}, retiredRoutes)
	require.Equal(t, []string{"a"}, addedRoutes)
}

func TestPlanInPlaceReload_StaleClaimDerivationChangeIsNotEligible(t *testing.T) {
	withGrace := func(outbox bool, grace string) *ports.BridgeConfig {
		cfg := reloadTestConfig("a", "b")
		if outbox {
			cfg.Stores.Outbox = &ports.StoreConfig{Type: "memory"}
		}
		cfg.Routes[0].Session = &ports.RouteSessionDef{SessionID: "a-s", SenderID: "a-tx", StepDownGrace: grace}
		return cfg
	}

	// The outbox store is handed the derived duration when it is opened, and an
	// in-place reload keeps it open.
	requireNotInPlace(t, withGrace(true, "30s"), withGrace(true, "90s"))

	// With no outbox store nothing is handed the duration, so the same route
	// change reloads in place.
	plan := mustPlanInPlace(t, withGrace(false, "30s"), withGrace(false, "90s"))
	retiredRoutes, addedRoutes, _, _ := plan.Summary()
	require.Equal(t, []string{"a"}, retiredRoutes)
	require.Equal(t, []string{"a"}, addedRoutes)
}

func TestPlanInPlaceReload_ExclusiveUnitSerializes(t *testing.T) {
	running := reloadTestConfig("a")
	addReloadTestOwner(running, "x", "exclusive")
	next := reloadTestConfig("a")
	addReloadTestOwner(next, "x", "exclusive")
	next.Bindings[1].Address = "topic/x-v2"

	plan := mustPlanInPlace(t, running, next)

	require.True(t, plan.Serialized())
}

// An exclusive unit the reload leaves alone does not serialize the reload of
// an ordinary one: only the retired and added units are asked about.
func TestPlanInPlaceReload_OrdinaryUnitIsNotSerialized(t *testing.T) {
	running := reloadTestConfig("a")
	addReloadTestOwner(running, "x", "exclusive")
	next := reloadTestConfig("a")
	addReloadTestOwner(next, "x", "exclusive")
	next.Bindings[0].Address = "topic/a-v2"

	plan := mustPlanInPlace(t, running, next)

	require.False(t, plan.Serialized())
}

func TestPlanInPlaceReload_NilConfigsAreNotEligible(t *testing.T) {
	requireNotInPlace(t, nil, reloadTestConfig("a"))
	requireNotInPlace(t, reloadTestConfig("a"), nil)
	requireNotInPlace(t, nil, nil)
}

func TestPlanInPlaceReload_SummaryListsRetiredAndAddedIDs(t *testing.T) {
	running := reloadTestConfig("a", "b", "c")
	next := reloadTestConfig("a", "c", "d")
	next.Bindings[0].Address = "topic/a-v2"

	plan := mustPlanInPlace(t, running, next)

	retiredRoutes, addedRoutes, retiredSessions, addedSessions := plan.Summary()
	require.Equal(t, []string{"a", "b"}, retiredRoutes)
	require.Equal(t, []string{"a", "d"}, addedRoutes)
	require.Equal(t, []string{"a-s", "b-s"}, retiredSessions)
	require.Equal(t, []string{"a-s", "d-s"}, addedSessions)
}

// The in-place eligibility check and the store the builder opens must read one
// stale-claim duration, or a reload could keep a store tuned for a config that
// is no longer running.
func TestDerivedStaleClaimDuration_IsWhatTheOutboxStoreIsHanded(t *testing.T) {
	cfg := reloadTestConfig("a")
	_, ok, err := derivedStaleClaimDuration(cfg)
	require.NoError(t, err)
	require.False(t, ok, "no outbox store is handed a duration")

	cfg.Stores.Outbox = &ports.StoreConfig{Type: "memory"}
	cfg.Routes[0].Session = &ports.RouteSessionDef{SessionID: "a-s", SenderID: "a-tx", StepDownGrace: "40s"}
	got, ok, err := derivedStaleClaimDuration(cfg)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 40*time.Second+80*time.Second, got)

	opts, err := NewBuilder(cfg).outboxRuntimeOptions(cfg.Stores.Outbox)
	require.NoError(t, err)
	require.Equal(t, opts.StaleClaimDuration, got)
}

// A lease TTL and a step-down grace near time.Duration's maximum parse and keep
// grace below TTL, so they are valid configuration. The derivation doubles and
// adds the grace; it must saturate at the maximum rather than wrap into a
// negative timeout the outbox store would be handed.
func TestDerivedStaleClaimDuration_SaturatesNearTheDurationLimit(t *testing.T) {
	cfg := reloadTestConfig("a")
	cfg.Stores.Outbox = &ports.StoreConfig{Type: "memory"}
	cfg.Routes[0].Session = &ports.RouteSessionDef{
		SessionID: "a-s", SenderID: "a-tx",
		LeaseTTL:      "2562047h47m16.854775807s",
		StepDownGrace: "2562047h47m16.854775806s",
	}

	got, ok, err := derivedStaleClaimDuration(cfg)

	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, time.Duration(math.MaxInt64), got)
}
