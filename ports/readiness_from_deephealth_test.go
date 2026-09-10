package ports_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/ports"
)

// TestReadinessLevelFromDeepHealth pins the pure snapshot→level derivation that
// /deephealth uses so the level is computed from ONE DeepHealth snapshot rather
// than a second live sweep (which could disagree with the snapshot it renders).
// Each case exercises one branch of the strict level ladder plus the standby
// cap and the deferred-connect skip.
func TestReadinessLevelFromDeepHealth(t *testing.T) {
	// connected builds a fully-connected+subscribed session.
	connected := func(id string) ports.SessionHealthDetail {
		return ports.SessionHealthDetail{
			SessionID: id, Connected: true,
			SubscriptionsWanted: 2, SubscriptionsActive: 2,
		}
	}

	cases := []struct {
		name string
		dh   ports.DeepHealth
		want ports.ReadinessLevel
	}{
		{
			name: "not running collapses to live",
			dh:   ports.DeepHealth{Running: false, Healthy: true},
			want: ports.LevelLive,
		},
		{
			name: "unhealthy collapses to live",
			dh:   ports.DeepHealth{Running: true, Healthy: false},
			want: ports.LevelLive,
		},
		{
			name: "disconnected session pins running",
			dh: ports.DeepHealth{
				Running: true, Healthy: true,
				Sessions: []ports.SessionHealthDetail{{SessionID: "s1", Connected: false}},
			},
			want: ports.LevelRunning,
		},
		{
			name: "connected but subscriptions incomplete pins connected",
			dh: ports.DeepHealth{
				Running: true, Healthy: true,
				Sessions: []ports.SessionHealthDetail{{
					SessionID: "s1", Connected: true,
					SubscriptionsWanted: 2, SubscriptionsActive: 1,
				}},
			},
			want: ports.LevelConnected,
		},
		{
			name: "equal aggregate counts but explicit subscription mismatch pins connected",
			dh: ports.DeepHealth{
				Running: true, Healthy: true,
				Sessions: []ports.SessionHealthDetail{{
					SessionID: "s1", Connected: true,
					SubscriptionsWanted: 1, SubscriptionsActive: 1,
					SubscriptionsSatisfied: boolPointer(false),
				}},
			},
			want: ports.LevelConnected,
		},
		{
			name: "subscribed but a route not ready pins subscribed",
			dh: ports.DeepHealth{
				Running: true, Healthy: true,
				Sessions: []ports.SessionHealthDetail{connected("s1")},
				Routes:   []ports.RouteHealth{{ID: "r1", Ready: false}},
			},
			want: ports.LevelSubscribed,
		},
		{
			// a latched-dead route (wedged flapping at the supervisor cap)
			// cannot dispatch, so it caps the instance below Full even though its
			// Started signal fired (Ready: true).
			name: "subscribed but a route latched dead pins subscribed",
			dh: ports.DeepHealth{
				Running: true, Healthy: true,
				Sessions: []ports.SessionHealthDetail{connected("s1")},
				Routes:   []ports.RouteHealth{{ID: "r1", Ready: true, RouteDead: true}},
			},
			want: ports.LevelSubscribed,
		},
		{
			name: "all connected+subscribed+routes-ready reaches full",
			dh: ports.DeepHealth{
				Running: true, Healthy: true,
				Sessions: []ports.SessionHealthDetail{connected("s1")},
				Routes:   []ports.RouteHealth{{ID: "r1", Ready: true}},
			},
			want: ports.LevelFull,
		},
		{
			// BLOCKING: a session that is connected+subscribed but EXPLICITLY
			// degraded (e.g. broker flow-control blocked) cannot fully serve, so
			// LevelFull's "ReadyForTraffic + ServiceLevelFull" contract caps it at
			// Subscribed even though every route is ready.
			name: "connected+subscribed but a session degraded pins subscribed",
			dh: ports.DeepHealth{
				Running: true, Healthy: true,
				Sessions: []ports.SessionHealthDetail{func() ports.SessionHealthDetail {
					s := connected("s1")
					s.ServiceLevel = ports.ServiceLevelDegraded
					return s
				}()},
				Routes: []ports.RouteHealth{{ID: "r1", Ready: true}},
			},
			want: ports.LevelSubscribed,
		},
		{
			// BLOCKING backward-compat: an UNSET (empty) ServiceLevel must NOT cap
			// — hand-built snapshots that leave it empty legitimately expect Full.
			name: "connected+subscribed with UNSET service level still reaches full",
			dh: ports.DeepHealth{
				Running: true, Healthy: true,
				Sessions: []ports.SessionHealthDetail{connected("s1")}, // ServiceLevel == ""
				Routes:   []ports.RouteHealth{{ID: "r1", Ready: true}},
			},
			want: ports.LevelFull,
		},
		{
			// BLOCKING: a DEFERRED-standby degraded session is skipped like the
			// connectivity checks, so it does not cap; the standby cap then lowers
			// the otherwise-Full instance to Subscribed independently.
			name: "deferred-standby degraded session is skipped from the service-level cap",
			dh: ports.DeepHealth{
				Running: true, Healthy: true, Role: "standby",
				Sessions: []ports.SessionHealthDetail{{
					SessionID: "s1", Connected: false,
					ConnectAfterLease: true, HasLease: false,
					ServiceLevel: ports.ServiceLevelNone,
				}},
			},
			want: ports.LevelSubscribed,
		},
		{
			name: "no sessions and no routes reaches full",
			dh:   ports.DeepHealth{Running: true, Healthy: true},
			want: ports.LevelFull,
		},
		{
			name: "standby caps an otherwise-full instance at subscribed",
			dh: ports.DeepHealth{
				Running: true, Healthy: true, Role: "standby",
				Sessions: []ports.SessionHealthDetail{connected("s1")},
				Routes:   []ports.RouteHealth{{ID: "r1", Ready: true}},
			},
			want: ports.LevelSubscribed,
		},
		{
			name: "deferred-connect standby without lease is skipped, not counted disconnected",
			dh: ports.DeepHealth{
				Running: true, Healthy: true, Role: "standby",
				Sessions: []ports.SessionHealthDetail{{
					SessionID: "s1", Connected: false,
					ConnectAfterLease: true, HasLease: false,
				}},
			},
			// Skipped session leaves allConnected/allSubscribed true; no routes →
			// would be Full, but the standby cap lowers it to Subscribed.
			want: ports.LevelSubscribed,
		},
		{
			name: "deferred-connect session WITH lease is not skipped",
			dh: ports.DeepHealth{
				Running: true, Healthy: true,
				Sessions: []ports.SessionHealthDetail{{
					SessionID: "s1", Connected: false,
					ConnectAfterLease: true, HasLease: true,
				}},
			},
			want: ports.LevelRunning,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ports.ReadinessLevelFromDeepHealth(tc.dh))
		})
	}
}

func boolPointer(value bool) *bool { return &value }
