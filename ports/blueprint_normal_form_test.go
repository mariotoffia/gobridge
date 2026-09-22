package ports_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// sharedPlugin is a pointer-typed PluginConfig so a test can prove the normal
// form carries the decoded plugin options through by identity, untouched.
type sharedPlugin struct{ broker string }

// Named types a hand-built rule could carry as a condition value. The runtime
// stringifies every one of them, so the normal form must as well.
type (
	namedInt    int
	namedBool   bool
	namedString string
	floatNamed  string
)

func (floatNamed) Float64() (float64, error) { return 1, nil }

func (*sharedPlugin) Kind() string    { return "shared" }
func (*sharedPlugin) Validate() error { return nil }

// normalFormFixture is a config written the way a generator might write it:
// every id-keyed list out of order, durations in mixed spellings, a version
// number, and every order-sensitive list deliberately NOT in sorted order.
func normalFormFixture(plugin ports.PluginConfig) *ports.BridgeConfig {
	s2 := ports.SessionDef{ID: "s2", Transport: "mqtt"}
	s1 := ports.SessionDef{ID: "s1", Transport: "mqtt"}
	s1.SetDecoded(plugin, nil)
	return &ports.BridgeConfig{
		Version: 7,
		Bridge: ports.BridgeSettings{
			ID:                    "demo",
			ShutdownTimeout:       "60000ms",
			DrainTimeout:          "30000ms",
			PerRecordDrainTimeout: "3000ms",
			Cluster: &ports.ClusterConfig{
				Members:       []string{"node-b", "node-a"},
				ConfirmWindow: "90000ms",
			},
		},
		ConfigWatch: &ports.ConfigWatchDef{Mode: "poll", PollInterval: "2000ms", Debounce: "500ms"},
		Sessions:    []ports.SessionDef{s2, s1},
		Receivers: []ports.ReceiverDef{
			{ID: "r2", Transport: "mqtt", Topics: []ports.SubscriptionDef{{Topic: "t/2"}, {Topic: "t/1"}}},
			{ID: "r1", Transport: "mqtt"},
		},
		Senders: []ports.SenderDef{{ID: "x2", Transport: "mqtt"}, {ID: "x1", Transport: "mqtt"}},
		Bindings: []ports.BindingDef{
			{ID: "b2", SenderID: "x2", Address: "out/2"},
			{ID: "b1", SenderID: "x1", Address: "out/1"},
		},
		Routes: []ports.RouteDef{
			{
				ID:         "route-b",
				ReceiverID: "r2",
				Bindings:   []string{"b2", "b1"},
				Processors: []string{"p2", "p1"},
				Policy: ports.PolicyDef{
					AckAfter:      "outbox_persist",
					ReplayBudget:  "120s",
					SendTimeout:   "5000ms",
					DepthCacheTTL: "250ms",
					Backoff:       ports.BackoffDef{InitialInterval: "100ms", MaxInterval: "60000ms"},
				},
				Resolver: &ports.ResolverDef{Type: "rules", Rules: []ports.RuleDef{{BindingID: "b2"}, {BindingID: "b1"}}},
				Session: &ports.RouteSessionDef{
					SessionID:            "s1",
					SenderID:             "x1",
					LeaseTTL:             "360000ms",
					RenewInterval:        "90s",
					RenewJitter:          "5000ms",
					StepDownGrace:        "2000ms",
					AcquirePollInterval:  "5s",
					RenewCallTimeout:     "3000ms",
					FailoverSLO:          "600s",
					StartupAllowance:     "0s",
					BrokerHealthStepDown: "off",
					DrainInterval:        "1000ms",
					DrainStrategy:        &ports.DrainStrategyDef{Type: "backoff", Interval: "1s", MinInterval: "100ms", MaxInterval: "30000ms"},
				},
			},
			{ID: "route-a", ReceiverID: "r1", Bindings: []string{"b1"}},
		},
	}
}

func idsOfSessions(defs []ports.SessionDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.ID)
	}
	return out
}

func TestContentNormalForm_NilIsNil(t *testing.T) {
	assert.Nil(t, ports.ContentNormalForm(nil))
}

func TestContentNormalForm_DropsVersion(t *testing.T) {
	got := ports.ContentNormalForm(normalFormFixture(nil))
	assert.Equal(t, 0, got.Version, "the version is a writer's counter, not something the bridge runs")
}

func TestContentNormalForm_SortsIDKeyedListsByID(t *testing.T) {
	got := ports.ContentNormalForm(normalFormFixture(nil))

	assert.Equal(t, []string{"s1", "s2"}, idsOfSessions(got.Sessions))
	assert.Equal(t, "r1", got.Receivers[0].ID)
	assert.Equal(t, "r2", got.Receivers[1].ID)
	assert.Equal(t, "x1", got.Senders[0].ID)
	assert.Equal(t, "x2", got.Senders[1].ID)
	assert.Equal(t, "b1", got.Bindings[0].ID)
	assert.Equal(t, "b2", got.Bindings[1].ID)
	assert.Equal(t, "route-a", got.Routes[0].ID)
	assert.Equal(t, "route-b", got.Routes[1].ID)
}

func TestContentNormalForm_SortIsStableForDuplicateIDs(t *testing.T) {
	cfg := &ports.BridgeConfig{Sessions: []ports.SessionDef{
		{ID: "dup", Transport: "second"},
		{ID: "a", Transport: "first"},
		{ID: "dup", Transport: "third"},
	}}
	got := ports.ContentNormalForm(cfg)
	require.Len(t, got.Sessions, 3)
	assert.Equal(t, "a", got.Sessions[0].ID)
	assert.Equal(t, "second", got.Sessions[1].Transport, "duplicates keep their written order")
	assert.Equal(t, "third", got.Sessions[2].Transport)
}

func TestContentNormalForm_KeepsOrderSensitiveListsPositional(t *testing.T) {
	got := ports.ContentNormalForm(normalFormFixture(nil))

	routeB := got.Routes[1]
	assert.Equal(t, []string{"b2", "b1"}, routeB.Bindings, "the first binding is the route's primary session")
	assert.Equal(t, []string{"p2", "p1"}, routeB.Processors, "a processor chain runs in order")
	assert.Equal(t, "b2", routeB.Resolver.Rules[0].BindingID, "rules are evaluated in order")
	assert.Equal(t, "t/2", got.Receivers[1].Topics[0].Topic, "subscriptions stay as written")
}

// The cluster roster is a set everywhere the bridge reads it (admission,
// preflight and the coordinator all sort it), so it is sorted here too and a
// reordered roster is the same cohort.
func TestContentNormalForm_SortsTheClusterRoster(t *testing.T) {
	input := normalFormFixture(nil)
	got := ports.ContentNormalForm(input)
	assert.Equal(t, []string{"node-a", "node-b"}, got.Bridge.Cluster.Members)
	assert.Equal(t, []string{"node-b", "node-a"}, input.Bridge.Cluster.Members, "the input is never modified")
}

func TestContentNormalForm_CanonicalDurationSpelling(t *testing.T) {
	got := ports.ContentNormalForm(normalFormFixture(nil))

	assert.Equal(t, "1m0s", got.Bridge.ShutdownTimeout)
	assert.Equal(t, "30s", got.Bridge.DrainTimeout)
	assert.Equal(t, "3s", got.Bridge.PerRecordDrainTimeout)
	assert.Equal(t, "1m30s", got.Bridge.Cluster.ConfirmWindow)
	assert.Equal(t, "2s", got.ConfigWatch.PollInterval)
	assert.Equal(t, "500ms", got.ConfigWatch.Debounce)

	route := got.Routes[1]
	assert.Equal(t, "outbox_persist", route.Policy.AckAfter, "ack_after is a keyword, not a duration; it is kept as written")
	assert.Equal(t, "2m0s", route.Policy.ReplayBudget)
	assert.Equal(t, "5s", route.Policy.SendTimeout)
	assert.Equal(t, "250ms", route.Policy.DepthCacheTTL)
	assert.Equal(t, "100ms", route.Policy.Backoff.InitialInterval)
	assert.Equal(t, "1m0s", route.Policy.Backoff.MaxInterval)

	session := route.Session
	assert.Equal(t, "6m0s", session.LeaseTTL)
	assert.Equal(t, "1m30s", session.RenewInterval)
	assert.Equal(t, "5s", session.RenewJitter)
	assert.Equal(t, "2s", session.StepDownGrace)
	assert.Equal(t, "5s", session.AcquirePollInterval)
	assert.Equal(t, "3s", session.RenewCallTimeout)
	assert.Equal(t, "10m0s", session.FailoverSLO)
	assert.Equal(t, "0s", session.StartupAllowance)
	assert.Equal(t, "off", session.BrokerHealthStepDown, "a non-duration keyword is a value of its own")
	assert.Equal(t, "1s", session.DrainInterval)
	assert.Equal(t, "1s", session.DrainStrategy.Interval)
	assert.Equal(t, "100ms", session.DrainStrategy.MinInterval)
	assert.Equal(t, "30s", session.DrainStrategy.MaxInterval)
}

// The two timeouts whose default ports itself defines (the *Duration accessors
// on BridgeSettings) are written out, so a document that leaves them out and one
// that writes the default compare equal. Defaults owned by other layers are not
// filled in here: an unset value stays unset.
func TestContentNormalForm_FillsTheDefaultsPortsOwns(t *testing.T) {
	got := ports.ContentNormalForm(&ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "demo"}})

	assert.Equal(t, "30s", got.Bridge.ShutdownTimeout)
	assert.Equal(t, "30s", got.Bridge.DrainTimeout)
	assert.Empty(t, got.Bridge.PerRecordDrainTimeout, "its default lives in the outbox drainer, not in ports")
	assert.Empty(t, got.Bridge.MaxDrainTimeout)
}

// An explicit zero is a value the validator rejects, so the normal form must
// keep it distinct from an omitted value: equating the two would let an invalid
// document be adopted as a no-op before validation sees it.
func TestContentNormalForm_ExplicitZeroIsNotTheDefault(t *testing.T) {
	got := ports.ContentNormalForm(&ports.BridgeConfig{Bridge: ports.BridgeSettings{
		ID:              "demo",
		ShutdownTimeout: "0s",
		DrainTimeout:    "0ms",
		Cluster:         &ports.ClusterConfig{Rollout: "coordinated", ConfirmWindow: "0s"},
	}})

	assert.Equal(t, "0s", got.Bridge.ShutdownTimeout)
	assert.Equal(t, "0s", got.Bridge.DrainTimeout)
	assert.Equal(t, "0s", got.Bridge.Cluster.ConfirmWindow)

	absent := ports.ContentNormalForm(&ports.BridgeConfig{Bridge: ports.BridgeSettings{
		ID:      "demo",
		Cluster: &ports.ClusterConfig{Rollout: "coordinated"},
	}})
	assert.NotEqual(t, absent, got)
}

// send_retry_budget is tri-state: an omitted budget takes the retry default
// while an explicit zero turns in-process send retry off, so the two are
// different configurations. Two spellings of one budget are the same one.
func TestContentNormalForm_SendRetryBudgetExplicitZeroIsNotOmitted(t *testing.T) {
	withBudget := func(budget string) *ports.BridgeConfig {
		return &ports.BridgeConfig{
			Bridge: ports.BridgeSettings{ID: "demo"},
			Routes: []ports.RouteDef{{ID: "r", Policy: ports.PolicyDef{SendRetryBudget: budget}}},
		}
	}

	disabled := ports.ContentNormalForm(withBudget("0s"))
	assert.Equal(t, "0s", disabled.Routes[0].Policy.SendRetryBudget)
	assert.NotEqual(t, ports.ContentNormalForm(withBudget("")), disabled,
		"turning in-process retry off is a change, not the default")

	assert.Equal(t, ports.ContentNormalForm(withBudget("60s")), ports.ContentNormalForm(withBudget("1m")))
	assert.Equal(t, "1m0s", ports.ContentNormalForm(withBudget("60000ms")).Routes[0].Policy.SendRetryBudget)
}

func TestContentNormalForm_KeepsUnparseableDurationAsWritten(t *testing.T) {
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "demo", DrainTimeout: "soon"}}
	got := ports.ContentNormalForm(cfg)
	assert.Equal(t, "soon", got.Bridge.DrainTimeout, "a value that cannot be normalised still counts, as written")
}

func TestContentNormalForm_DoesNotMutateInput(t *testing.T) {
	plugin := &sharedPlugin{broker: "tcp://a:1883"}
	input := normalFormFixture(plugin)
	untouched := normalFormFixture(plugin)

	got := ports.ContentNormalForm(input)

	require.Equal(t, untouched, input, "the input keeps its version, order and spellings")
	assert.NotSame(t, input, got)
	assert.NotSame(t, input.Bridge.Cluster, got.Bridge.Cluster)
	assert.NotSame(t, input.ConfigWatch, got.ConfigWatch)
	assert.NotSame(t, input.Routes[0].Session, got.Routes[1].Session)
	assert.NotSame(t, input.Routes[0].Session.DrainStrategy, got.Routes[1].Session.DrainStrategy)
}

func TestContentNormalForm_CarriesPluginConfigByIdentity(t *testing.T) {
	plugin := &sharedPlugin{broker: "tcp://a:1883"}
	got := ports.ContentNormalForm(normalFormFixture(plugin))

	require.Equal(t, "s1", got.Sessions[0].ID)
	assert.Same(t, plugin, got.Sessions[0].Config, "plugin options are the adapter's; the normal form never rewrites them")
}

// The property the whole design rests on: two documents that mean the same
// thing have the same normal form. This is the table from the issue that
// motivated it — a raised version number, sessions listed in the opposite
// order, a default left out versus written out, and two spellings of one
// duration — plus the reverse case, so a real change still differs.
func TestContentNormalForm_EquivalentDocumentsAgree(t *testing.T) {
	handwritten := &ports.BridgeConfig{
		Version: 3,
		Bridge:  ports.BridgeSettings{ID: "demo"},
		Sessions: []ports.SessionDef{
			{ID: "a", Transport: "mqtt"},
			{ID: "b", Transport: "mqtt"},
		},
	}
	generated := &ports.BridgeConfig{
		Version: 4,
		Bridge:  ports.BridgeSettings{ID: "demo", DrainTimeout: "30000ms", ShutdownTimeout: "30s"},
		Sessions: []ports.SessionDef{
			{ID: "b", Transport: "mqtt"},
			{ID: "a", Transport: "mqtt"},
		},
	}
	assert.Equal(t, ports.ContentNormalForm(handwritten), ports.ContentNormalForm(generated))

	changed := &ports.BridgeConfig{
		Version: 4,
		Bridge:  ports.BridgeSettings{ID: "demo", DrainTimeout: "31s"},
		Sessions: []ports.SessionDef{
			{ID: "b", Transport: "mqtt"},
			{ID: "a", Transport: "mqtt"},
		},
	}
	assert.NotEqual(t, ports.ContentNormalForm(handwritten), ports.ContentNormalForm(changed))
}

// A rule's condition value is compared the way the runtime coerces it: nil,
// scalars and lists keep their kind, and anything else (a map, for instance)
// is the string fmt.Sprint gives, so an empty map and an absent value are two
// different rules, exactly as they are two different matches at runtime. An
// empty list is written the same way so it stays distinct from an absent
// value, which the projection would otherwise not tell apart.
func TestContentNormalForm_ConditionValuesFollowTheRuntimeCoercion(t *testing.T) {
	withValue := func(v any) *ports.BridgeConfig {
		return &ports.BridgeConfig{Routes: []ports.RouteDef{{
			ID: "r",
			Resolver: &ports.ResolverDef{Type: "rules", Rules: []ports.RuleDef{{
				BindingID: "b",
				Match:     []ports.ConditionDef{{Field: "kind", Operator: "eq", Value: v}},
			}}},
		}}}
	}
	valueOf := func(cfg *ports.BridgeConfig) any {
		return ports.ContentNormalForm(cfg).Routes[0].Resolver.Rules[0].Match[0].Value
	}

	assert.Nil(t, valueOf(withValue(nil)))
	assert.Equal(t, "x", valueOf(withValue("x")))
	assert.Equal(t, float64(7), valueOf(withValue(7)), "every number is the float64 the runtime compares")
	assert.Equal(t, float64(7), valueOf(withValue(int64(7))))
	assert.Equal(t, true, valueOf(withValue(true)))
	assert.Equal(t, []any{"1", float64(2)}, valueOf(withValue([]any{"1", 2})), "a list keeps its element kinds, numbers as float64")
	assert.Equal(t, "map[]", valueOf(withValue(map[string]any{})))
	assert.Equal(t, "map[a:x b:1]", valueOf(withValue(map[string]any{"b": 1, "a": "x"})))
	emptyList := valueOf(withValue([]any{}))
	assert.NotNil(t, emptyList, "an empty list is not an absent value")
	assert.NotEqual(t, valueOf(withValue("[]")), emptyList, "nor is it the string \"[]\"")
	assert.NotEqual(t, valueOf(withValue([]any{"x"})), emptyList)
	_, isSlice := emptyList.([]any)
	assert.False(t, isSlice, "an empty list becomes a marker no document can carry, so a projection cannot fold it into an absent value")
	assert.Equal(t, []any{"map[]"}, valueOf(withValue([]any{map[string]any{}})), "the rule reaches into list elements")

	// Two integers the runtime cannot tell apart are one rule here as well, so
	// an update between them is not a change.
	assert.Equal(t, valueOf(withValue(int64(9007199254740992))), valueOf(withValue(int64(9007199254740993))))

	// The runtime compares -0.0 and 0.0 as one float; the projection would write
	// them as "-0" and "0", so the normal form writes both as the plain zero.
	negativeZero := math.Copysign(0, -1)
	assert.Equal(t, float64(0), valueOf(withValue(negativeZero)))
	assert.True(t, math.Signbit(negativeZero), "the fixture really is a negative zero")
	assert.False(t, math.Signbit(valueOf(withValue(negativeZero)).(float64)))
	assert.Equal(t, []any{float64(0)}, valueOf(withValue([]float64{negativeZero})))

	// Only the exact concrete types the runtime special-cases are numbers or
	// bools here; a named type, or any other type with a Float64 method, is
	// what the runtime makes of it, the string fmt.Sprint gives.
	assert.Equal(t, float64(7), valueOf(withValue(json.Number("7"))), "a decoded JSON number is the float the runtime compares")
	assert.Equal(t, "7", valueOf(withValue(namedInt(7))))
	assert.Equal(t, "true", valueOf(withValue(namedBool(true))))
	assert.Equal(t, "alpha", valueOf(withValue(floatNamed("alpha"))), "a Float64 method on another type means nothing to the runtime")
	assert.Equal(t, "alpha", valueOf(withValue(namedString("alpha"))))

	// The typed lists the runtime knows become lists of the same normalised
	// elements; any other slice is what the runtime makes of it, a string.
	assert.Equal(t, []any{float64(1)}, valueOf(withValue([]int{1})))
	assert.Equal(t, []any{"a"}, valueOf(withValue([]string{"a"})))
	assert.Equal(t, valueOf(withValue([]any{})), valueOf(withValue([]string{})), "every empty list the runtime knows is the same marker")
	assert.Equal(t, "[1 2]", valueOf(withValue([]int64{1, 2})))

	input := withValue(map[string]any{"k": "v"})
	ports.ContentNormalForm(input)
	assert.Equal(t, map[string]any{"k": "v"}, input.Routes[0].Resolver.Rules[0].Match[0].Value, "the input is never modified")
}

// Normalising a normal form changes nothing, so a caller that hands an already
// normalised config to a digest gets the same identity. The marker for an empty
// condition list is the value most at risk of being rewritten on a second pass.
func TestContentNormalForm_IsIdempotent(t *testing.T) {
	cfg := normalFormFixture(nil)
	cfg.Routes[0].Resolver.Rules = append(cfg.Routes[0].Resolver.Rules, ports.RuleDef{
		BindingID: "b1",
		Match: []ports.ConditionDef{
			{Field: "tags", Operator: "in", Value: []any{}},
			{Field: "meta", Operator: "eq", Value: map[string]any{}},
			{Field: "n", Operator: "eq", Value: 7},
		},
	})
	once := ports.ContentNormalForm(cfg)
	twice := ports.ContentNormalForm(once)
	assert.Equal(t, once, twice)
}
