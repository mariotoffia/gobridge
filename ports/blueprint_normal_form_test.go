package ports_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// sharedPlugin is a pointer-typed PluginConfig so a test can prove the normal
// form carries the decoded plugin options through by identity, untouched.
type sharedPlugin struct{ broker string }

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
					AckAfter:      "1000ms",
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
	assert.Equal(t, []string{"node-b", "node-a"}, got.Bridge.Cluster.Members, "the roster stays as written")
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
	assert.Equal(t, "1s", route.Policy.AckAfter)
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

func TestContentNormalForm_ConfirmWindowZeroAndAbsentAgree(t *testing.T) {
	absent := ports.ContentNormalForm(&ports.BridgeConfig{Bridge: ports.BridgeSettings{
		Cluster: &ports.ClusterConfig{Rollout: "coordinated"},
	}})
	zero := ports.ContentNormalForm(&ports.BridgeConfig{Bridge: ports.BridgeSettings{
		Cluster: &ports.ClusterConfig{Rollout: "coordinated", ConfirmWindow: "0s"},
	}})
	assert.Equal(t, absent.Bridge.Cluster, zero.Bridge.Cluster, "both select the base protocol")
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
