package paho

import (
	"context"
	"log/slog"
	"testing"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

func TestHealth_ServiceLevel(t *testing.T) {
	tests := []struct {
		name       string
		connected  bool // cm != nil
		wantedSubs int  // len(plan.Subscriptions)
		activeSubs int  // len(activeSubs)
		handlers   int  // registered handler count
		wantReady  bool
		wantSL     ports.ServiceLevel
	}{
		{
			name:      "disconnected",
			connected: false,
			wantReady: false,
			wantSL:    ports.ServiceLevelNone,
		},
		{
			name:      "connected sender-only (no plan)",
			connected: true,
			wantReady: true,
			wantSL:    ports.ServiceLevelFull,
		},
		{
			name:       "connected all subs active with handlers",
			connected:  true,
			wantedSubs: 3,
			activeSubs: 3,
			handlers:   1,
			wantReady:  true,
			wantSL:     ports.ServiceLevelFull,
		},
		{
			name:       "connected partial subs active",
			connected:  true,
			wantedSubs: 3,
			activeSubs: 1,
			handlers:   1,
			wantReady:  true,
			wantSL:     ports.ServiceLevelDegraded,
		},
		{
			name:       "connected no subs no handlers",
			connected:  true,
			wantedSubs: 3,
			activeSubs: 0,
			handlers:   0,
			wantReady:  true,
			wantSL:     ports.ServiceLevelNone,
		},
		{
			name:       "connected all subs active but no handlers",
			connected:  true,
			wantedSubs: 2,
			activeSubs: 2,
			handlers:   0,
			wantReady:  true,
			wantSL:     ports.ServiceLevelDegraded,
		},
		{
			name:       "connected some subs no handlers",
			connected:  true,
			wantedSubs: 3,
			activeSubs: 2,
			handlers:   0,
			wantReady:  true,
			wantSL:     ports.ServiceLevelDegraded,
		},
		{
			name:       "connected with handlers but zero active subs",
			connected:  true,
			wantedSubs: 2,
			activeSubs: 0,
			handlers:   1,
			wantReady:  true,
			wantSL:     ports.ServiceLevelDegraded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger := slog.Default()
			s := NewSession(SessionOptions{}, connectivity.SessionEphemeral, logger)

			// Simulate connection state.
			if tc.connected {
				s.mu.Lock()
				s.cm = &pahoConn{cm: &autopaho.ConnectionManager{}}
				s.connected = true
				s.mu.Unlock()
			}

			// Set plan with desired subscription count.
			if tc.wantedSubs > 0 {
				plan := connectivity.SessionPlan{
					Subscriptions: make([]connectivity.SubscriptionPlan, tc.wantedSubs),
				}
				for i := range plan.Subscriptions {
					plan.Subscriptions[i] = connectivity.SubscriptionPlan{
						Topic: "test/topic/" + string(rune('a'+i)),
						QoS:   1,
					}
				}
				s.mu.Lock()
				s.plan = &plan
				s.mu.Unlock()
			}

			// Populate active subscriptions.
			if tc.activeSubs > 0 {
				s.mu.Lock()
				for i := range tc.activeSubs {
					s.activeSubs["test/topic/"+string(rune('a'+i))] = 1
				}
				s.mu.Unlock()
			}

			// Register handlers on the router.
			for i := range tc.handlers {
				s.router.Register("handler-"+string(rune('0'+i)),
					func(*pahov5.Publish) {})
			}

			h := s.Health(context.Background())
			assert.Equal(t, tc.wantReady, h.Ready, "Ready mismatch")
			assert.Equal(t, tc.wantSL, h.ServiceLevel, "ServiceLevel mismatch")
			assert.Equal(t, tc.wantedSubs, h.SubscriptionsWanted)
			assert.Equal(t, tc.activeSubs, h.SubscriptionsActive)
			assert.Equal(t, tc.handlers, h.HandlersRegistered)
		})
	}
}

func TestHealth_ServiceLevelFull_RequiresEveryExpectedReceiverHandler(t *testing.T) {
	s := fullHealthSession(t, connectivity.SessionPlan{
		Subscriptions:       []connectivity.SubscriptionPlan{{Topic: "orders", QoS: 1}},
		ExpectedReceiverIDs: []string{"rx-orders", "rx-audit"},
	}, map[string]byte{"orders": 1}, "rx-orders")

	assert.Equal(t, ports.ServiceLevelDegraded, s.Health(context.Background()).ServiceLevel)
}

func TestHealth_ServiceLevelFull_UnrelatedHandlerDoesNotSatisfyExpectedReceiver(t *testing.T) {
	s := fullHealthSession(t, connectivity.SessionPlan{
		Subscriptions:       []connectivity.SubscriptionPlan{{Topic: "orders", QoS: 1}},
		ExpectedReceiverIDs: []string{"rx-orders"},
	}, map[string]byte{"orders": 1}, "diagnostic-tap")

	assert.Equal(t, ports.ServiceLevelDegraded, s.Health(context.Background()).ServiceLevel)
}

func TestHealth_ServiceLevelFull_DeduplicatesDesiredTopicFilters(t *testing.T) {
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "orders", QoS: 0},
			{Topic: "orders", QoS: 1},
		},
		ExpectedReceiverIDs: []string{"rx-orders"},
	}

	below := fullHealthSession(t, plan, map[string]byte{"orders": 0}, "rx-orders")
	assert.Equal(t, ports.ServiceLevelDegraded, below.Health(context.Background()).ServiceLevel,
		"a duplicate filter retains its strongest requested QoS")

	atRequired := fullHealthSession(t, plan, map[string]byte{"orders": 1}, "rx-orders")
	h := atRequired.Health(context.Background())
	assert.Equal(t, ports.ServiceLevelFull, h.ServiceLevel)
	assert.Equal(t, 1, h.SubscriptionsWanted)
	assert.Equal(t, 1, h.SubscriptionsActive)
}

func TestHealth_ServiceLevelFull_RequiresActualFilterAndQoSAtBoundary(t *testing.T) {
	tests := []struct {
		name   string
		active map[string]byte
		want   ports.ServiceLevel
	}{
		{name: "different filter at same aggregate count", active: map[string]byte{"other": 1}, want: ports.ServiceLevelDegraded},
		{name: "granted below requested", active: map[string]byte{"orders": 0}, want: ports.ServiceLevelDegraded},
		{name: "granted equals requested", active: map[string]byte{"orders": 1}, want: ports.ServiceLevelFull},
		{name: "granted exceeds requested", active: map[string]byte{"orders": 2}, want: ports.ServiceLevelFull},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := fullHealthSession(t, connectivity.SessionPlan{
				Subscriptions:       []connectivity.SubscriptionPlan{{Topic: "orders", QoS: 1}},
				ExpectedReceiverIDs: []string{"rx-orders"},
			}, tc.active, "rx-orders")

			assert.Equal(t, tc.want, s.Health(context.Background()).ServiceLevel)
		})
	}
}

func TestRouter_HandlerIDsReturnsSortedSnapshot(t *testing.T) {
	r := newRouter(nil, nil)
	r.Register("rx-z", func(*pahov5.Publish) {})
	r.Register("rx-a", func(*pahov5.Publish) {})

	ids := r.HandlerIDs()
	assert.Equal(t, []string{"rx-a", "rx-z"}, ids)

	ids[0] = "mutated"
	assert.Equal(t, []string{"rx-a", "rx-z"}, r.HandlerIDs())
}

func fullHealthSession(t *testing.T, plan connectivity.SessionPlan, active map[string]byte, handlerIDs ...string) *Session {
	t.Helper()
	s := NewSession(SessionOptions{}, connectivity.SessionEphemeral, slog.Default())
	s.mu.Lock()
	s.cm = &pahoConn{cm: &autopaho.ConnectionManager{}}
	s.connected = true
	s.plan = &plan
	s.activeSubs = active
	s.mu.Unlock()
	for _, id := range handlerIDs {
		s.router.Register(id, func(*pahov5.Publish) {})
	}
	return s
}
