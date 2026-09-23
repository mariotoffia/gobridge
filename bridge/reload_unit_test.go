package bridge

import (
	"slices"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

// reloadTestConfig returns a config with one independent owner per id (see
// addReloadTestOwner), each on the "fake" transport.
func reloadTestConfig(owners ...string) *ports.BridgeConfig {
	cfg := &ports.BridgeConfig{
		Version: 1,
		Bridge:  ports.BridgeSettings{ID: "test-bridge", DrainTimeout: "1s"},
		Stores:  ports.StoresConfig{Lease: &ports.StoreConfig{Type: "memory"}},
	}
	for _, owner := range owners {
		addReloadTestOwner(cfg, owner, "fake")
	}
	return cfg
}

// addReloadTestOwner appends the pipe one owner holds: a session on transport,
// a receiver and a sender on that session, a binding to the sender and a route
// from the receiver through the binding. The route is named after the owner.
func addReloadTestOwner(cfg *ports.BridgeConfig, owner, transport string) {
	cfg.Sessions = append(cfg.Sessions, ports.SessionDef{ID: owner + "-s", Transport: transport})
	cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{ID: owner + "-rx", SessionID: owner + "-s"})
	cfg.Senders = append(cfg.Senders, ports.SenderDef{ID: owner + "-tx", SessionID: owner + "-s"})
	cfg.Bindings = append(cfg.Bindings, ports.BindingDef{ID: owner + "-b", SenderID: owner + "-tx", Address: "topic/" + owner})
	cfg.Routes = append(cfg.Routes, ports.RouteDef{
		ID:           owner,
		ReceiverID:   owner + "-rx",
		DeliveryMode: "direct_hold",
		Bindings:     []string{owner + "-b"},
	})
}

func mustSplitUnits(t *testing.T, cfg *ports.BridgeConfig) []reloadUnit {
	t.Helper()
	units, err := splitUnits(cfg)
	require.NoError(t, err)
	return units
}

func unitWithRoute(t *testing.T, units []reloadUnit, route string) reloadUnit {
	t.Helper()
	for _, u := range units {
		if slices.Contains(u.routes, route) {
			return u
		}
	}
	t.Fatalf("no reload unit holds route %q", route)
	return reloadUnit{}
}

// memberIDs lists a sub-config's members in the order they are written, each
// prefixed with its collection.
func memberIDs(sub *ports.BridgeConfig) []string {
	var ids []string
	for i := range sub.Sessions {
		ids = append(ids, "session:"+sub.Sessions[i].ID)
	}
	for i := range sub.Receivers {
		ids = append(ids, "receiver:"+sub.Receivers[i].ID)
	}
	for i := range sub.Senders {
		ids = append(ids, "sender:"+sub.Senders[i].ID)
	}
	for i := range sub.Bindings {
		ids = append(ids, "binding:"+sub.Bindings[i].ID)
	}
	for i := range sub.Routes {
		ids = append(ids, "route:"+sub.Routes[i].ID)
	}
	return ids
}

func TestSplitUnits_RoutesSharingASessionAreOneUnit(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge:   ports.BridgeSettings{ID: "test-bridge"},
		Sessions: []ports.SessionDef{{ID: "shared", Transport: "fake"}},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", SessionID: "shared"},
			{ID: "rx2", SessionID: "shared"},
		},
		Senders: []ports.SenderDef{{ID: "tx1", Transport: "fake"}, {ID: "tx2", Transport: "fake"}},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "t1"},
			{ID: "b2", SenderID: "tx2", Address: "t2"},
		},
		Routes: []ports.RouteDef{
			{ID: "r2", ReceiverID: "rx2", Bindings: []string{"b2"}},
			{ID: "r1", ReceiverID: "rx1", Bindings: []string{"b1"}},
		},
	}

	units := mustSplitUnits(t, cfg)

	require.Len(t, units, 1)
	require.Equal(t, []string{"r1", "r2"}, units[0].routes)
	require.Equal(t, []string{"shared"}, units[0].sessions)
	// The sub-config keeps the members in the order the document writes them.
	require.Equal(t, []string{
		"session:shared", "receiver:rx1", "receiver:rx2", "sender:tx1", "sender:tx2",
		"binding:b1", "binding:b2", "route:r2", "route:r1",
	}, memberIDs(units[0].sub))
}

func TestSplitUnits_IndependentOwnersAreSeparateUnits(t *testing.T) {
	cfg := reloadTestConfig("a", "b")

	units := mustSplitUnits(t, cfg)

	require.Len(t, units, 2)
	a, b := unitWithRoute(t, units, "a"), unitWithRoute(t, units, "b")
	require.Equal(t, []string{"a-s"}, a.sessions)
	require.Equal(t, []string{"b-s"}, b.sessions)
	require.Equal(t, []string{"session:a-s", "receiver:a-rx", "sender:a-tx", "binding:a-b", "route:a"}, memberIDs(a.sub))
	require.Equal(t, []string{"session:b-s", "receiver:b-rx", "sender:b-tx", "binding:b-b", "route:b"}, memberIDs(b.sub))
	require.NotEqual(t, a.key, b.key)

	// Every unit carries the bridge-wide sections, so it builds on its own.
	require.Equal(t, cfg.Bridge, a.sub.Bridge)
	require.Same(t, cfg.Stores.Lease, a.sub.Stores.Lease)
	require.Equal(t, cfg.Version, a.sub.Version)

	// The source config is left as it was.
	require.Len(t, cfg.Routes, 2)
	require.Len(t, cfg.Sessions, 2)
}

func TestSplitUnits_SharedSenderJoinsRoutes(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "test-bridge"},
		Receivers: []ports.ReceiverDef{{ID: "rx1", Transport: "fake"}, {ID: "rx2", Transport: "fake"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "fake"}},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx", Address: "t1"},
			{ID: "b2", SenderID: "tx", Address: "t2"},
		},
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "rx1", Bindings: []string{"b1"}},
			{ID: "r2", ReceiverID: "rx2", Bindings: []string{"b2"}},
		},
	}

	units := mustSplitUnits(t, cfg)

	require.Len(t, units, 1)
	require.Equal(t, []string{"r1", "r2"}, units[0].routes)
	require.Empty(t, units[0].sessions)
}

func TestSplitUnits_BindingSessionJoinsRoute(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "test-bridge"},
		Sessions:  []ports.SessionDef{{ID: "lease-s", Transport: "fake"}},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "fake"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "fake"}},
		Bindings:  []ports.BindingDef{{ID: "b", SenderID: "tx", SessionID: "lease-s", Address: "t"}},
		Routes:    []ports.RouteDef{{ID: "r", ReceiverID: "rx", Bindings: []string{"b"}}},
	}

	units := mustSplitUnits(t, cfg)

	require.Len(t, units, 1)
	require.Equal(t, []string{"r"}, units[0].routes)
	require.Equal(t, []string{"lease-s"}, units[0].sessions)
	require.Contains(t, memberIDs(units[0].sub), "session:lease-s")
}

func TestSplitUnits_RouteSessionBlockJoinsSession(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "test-bridge"},
		Sessions:  []ports.SessionDef{{ID: "lease-s", Transport: "fake"}},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "fake"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "fake"}, {ID: "lease-tx", Transport: "fake"}},
		Bindings:  []ports.BindingDef{{ID: "b", SenderID: "tx", Address: "t"}},
		Routes: []ports.RouteDef{{
			ID:         "r",
			ReceiverID: "rx",
			Bindings:   []string{"b"},
			Session:    &ports.RouteSessionDef{SessionID: "lease-s", SenderID: "lease-tx"},
		}},
	}

	units := mustSplitUnits(t, cfg)

	require.Len(t, units, 1)
	require.Equal(t, []string{"lease-s"}, units[0].sessions)
	require.Equal(t, []string{
		"session:lease-s", "receiver:rx", "sender:tx", "sender:lease-tx", "binding:b", "route:r",
	}, memberIDs(units[0].sub))
}

// A session id nothing declares is still one the runtime can register a
// manager under, so retiring the unit must name it.
func TestSplitUnits_UndeclaredSessionIDsAreListed(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "test-bridge"},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "fake", SessionID: "rx-s"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "fake", SessionID: "tx-s"}},
		Bindings:  []ports.BindingDef{{ID: "b", SenderID: "tx", SessionID: "binding-s", Address: "t"}},
		Routes: []ports.RouteDef{
			{
				ID:         "r1",
				ReceiverID: "rx",
				Bindings:   []string{"b"},
				Session:    &ports.RouteSessionDef{SessionID: "lease-s", SenderID: "tx"},
			},
			// A second route naming the same undeclared session joins the unit.
			{ID: "r2", Session: &ports.RouteSessionDef{SessionID: "lease-s"}},
		},
	}

	units := mustSplitUnits(t, cfg)

	require.Len(t, units, 1)
	require.Equal(t, []string{"r1", "r2"}, units[0].routes)
	require.Equal(t, []string{"binding-s", "lease-s", "rx-s", "tx-s"}, units[0].sessions)
	require.Empty(t, units[0].sub.Sessions)
}

func TestSplitUnits_KeyIgnoresOrderAndVersion(t *testing.T) {
	cfg := reloadTestConfig("a", "b")
	reordered := reloadTestConfig("b", "a")
	reordered.Version = cfg.Version + 5

	units, reorderedUnits := mustSplitUnits(t, cfg), mustSplitUnits(t, reordered)

	for _, route := range []string{"a", "b"} {
		require.Equal(t, unitWithRoute(t, units, route).key, unitWithRoute(t, reorderedUnits, route).key, "route %q", route)
	}
}

func TestSplitUnits_KeyChangesWithMemberContent(t *testing.T) {
	cfg := reloadTestConfig("a", "b")
	changed := reloadTestConfig("a", "b")
	changed.Bindings[0].Address = "topic/a-v2"

	units, changedUnits := mustSplitUnits(t, cfg), mustSplitUnits(t, changed)

	require.NotEqual(t, unitWithRoute(t, units, "a").key, unitWithRoute(t, changedUnits, "a").key)
	require.Equal(t, unitWithRoute(t, units, "b").key, unitWithRoute(t, changedUnits, "b").key)
}

func TestUnionSub_HoldsEveryMemberOfTheUnits(t *testing.T) {
	cfg := reloadTestConfig("a", "b", "c")
	units := mustSplitUnits(t, cfg)

	union := unionSub(cfg, []reloadUnit{unitWithRoute(t, units, "a"), unitWithRoute(t, units, "c")})

	require.Equal(t, []string{
		"session:a-s", "session:c-s", "receiver:a-rx", "receiver:c-rx", "sender:a-tx", "sender:c-tx",
		"binding:a-b", "binding:c-b", "route:a", "route:c",
	}, memberIDs(union))
	require.Equal(t, cfg.Bridge, union.Bridge)
	require.Len(t, cfg.Routes, 3, "the base config is left as it was")
}

func TestStripUnits_KeepsOnlyTheBridgeWideSections(t *testing.T) {
	cfg := reloadTestConfig("a")

	stripped := stripUnits(cfg)

	require.Empty(t, memberIDs(stripped))
	require.Equal(t, cfg.Bridge, stripped.Bridge)
	require.Same(t, cfg.Stores.Lease, stripped.Stores.Lease)
	require.Len(t, cfg.Routes, 1, "the source config is left as it was")
	require.Nil(t, stripUnits(nil))
}
