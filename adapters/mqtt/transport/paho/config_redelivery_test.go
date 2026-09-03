package paho_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// What an MQTT route has to look like before the bridge may leave a message
// unsettled on it.
//
// The adapter acknowledges manually, so the PUBACK is withheld until the
// delivery settles — but that only helps if the broker still has the delivery
// after the process dies. It does when the session survives the process and the
// subscription is at-least-once, and it does not otherwise, and both are per-route
// choices rather than transport facts. These pin both directions, because a rule
// that only ever says yes admits exactly the routes it was written to refuse.

func redeliverySession(mode connectivity.SessionMode, cleanStart bool, expiry uint32) ports.SessionSpec {
	cfg := paho.DefaultConfig()
	cfg.Session.BrokerURLs = []string{"tcp://broker:1883"}
	cfg.Session.ClientID = "gateway"
	cfg.Session.CleanStart = cleanStart
	cfg.Session.SessionExpiryInterval = expiry
	return ports.SessionSpec{ID: "ingress", Transport: "mqtt", SessionMode: mode, Config: &cfg}
}

func subscribedAt(qos int) []connectivity.SubscriptionPlan {
	return []connectivity.SubscriptionPlan{{Topic: "sensors/#", QoS: qos}}
}

func TestSourceRedeliversUnsettled_DurableSessionAtLeastOnce(t *testing.T) {
	receiver := paho.DefaultConfig()

	for name, session := range map[string]ports.SessionSpec{
		"a persistent session that resumes": redeliverySession(connectivity.SessionPersistent, false, 3600),
		"an exclusive session":              redeliverySession(connectivity.SessionExclusive, false, 3600),
		"an exclusive session whose expiry is defaulted": redeliverySession(connectivity.SessionExclusive, false, 0),
	} {
		t.Run(name, func(t *testing.T) {
			redelivers, reason := receiver.SourceRedeliversUnsettled(session, subscribedAt(1))
			require.True(t, redelivers, "reason: %s", reason)
			require.Empty(t, reason)
		})
	}
}

func TestSourceRedeliversUnsettled_RefusesWhatCannotComeBack(t *testing.T) {
	receiver := paho.DefaultConfig()

	for name, tc := range map[string]struct {
		session ports.SessionSpec
		subs    []connectivity.SubscriptionPlan
		names   string
	}{
		"a QoS 0 subscription is delivered once and never repeated": {
			session: redeliverySession(connectivity.SessionPersistent, false, 3600),
			subs:    subscribedAt(0),
			names:   "QoS 0",
		},
		"a persistent session that clean-starts throws the session away": {
			session: redeliverySession(connectivity.SessionPersistent, true, 3600),
			subs:    subscribedAt(1),
			names:   "clean_start",
		},
		"an ephemeral session keeps nothing at all": {
			session: redeliverySession(connectivity.SessionEphemeral, false, 3600),
			subs:    subscribedAt(1),
			names:   "clean_start",
		},
		"a receiver with no subscriptions has no guarantee to inherit": {
			session: redeliverySession(connectivity.SessionPersistent, false, 3600),
			subs:    nil,
			names:   "no subscriptions",
		},
	} {
		t.Run(name, func(t *testing.T) {
			redelivers, reason := receiver.SourceRedeliversUnsettled(tc.session, tc.subs)
			require.False(t, redelivers)
			require.Contains(t, reason, tc.names,
				"the refusal has to name WHICH precondition failed; the two have different fixes")
		})
	}
}

// TestSourceRedeliversUnsettled_MixedQoSTakesTheWeakest pins the rule for a
// receiver with several subscriptions: the route inherits the guarantee of its
// weakest one, because a message arriving on the QoS 0 filter is the one that
// will not come back.
func TestSourceRedeliversUnsettled_MixedQoSTakesTheWeakest(t *testing.T) {
	receiver := paho.DefaultConfig()
	session := redeliverySession(connectivity.SessionPersistent, false, 3600)

	redelivers, reason := receiver.SourceRedeliversUnsettled(session, []connectivity.SubscriptionPlan{
		{Topic: "sensors/#", QoS: 1},
		{Topic: "telemetry/#", QoS: 0},
	})
	require.False(t, redelivers)
	require.Contains(t, reason, "telemetry/#")
}

// TestSourceRedeliversUnsettled_UnknownSessionIsRefused keeps the answer fail-
// closed: a session this adapter cannot read is not a session it can vouch for.
func TestSourceRedeliversUnsettled_UnknownSessionIsRefused(t *testing.T) {
	receiver := paho.DefaultConfig()

	redelivers, reason := receiver.SourceRedeliversUnsettled(
		ports.SessionSpec{ID: "ingress", SessionMode: connectivity.SessionPersistent}, subscribedAt(1))
	require.False(t, redelivers)
	require.Contains(t, reason, "no MQTT configuration")
}
