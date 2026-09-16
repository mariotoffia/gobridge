package paho

import (
	"fmt"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// Whether an MQTT source redelivers a message the bridge never settled.
//
// It is not a property of the transport, which is why the Factory does not
// declare it. The adapter acknowledges manually, so a PUBACK is withheld until
// the delivery settles — but whether the broker ever sends that delivery AGAIN
// after the process dies depends on two things the operator chooses per route:
//
//   - the SESSION must survive the process. A broker keeps unacknowledged QoS 1
//     deliveries in the session state it holds for a client id; a session that is
//     discarded on connect, or that the broker is told to keep for zero seconds,
//     takes them with it.
//   - the SUBSCRIPTION must be QoS 1 or 2. QoS 0 is at-most-once: the broker
//     sends once and keeps nothing, so there is nothing to redeliver.
//
// Both must hold for source redelivery. Best-effort direct_hold admission is
// separate: accepting QoS 0 must not claim recovery or waive failure-sink checks.

var _ ports.SourceRedeliveryConfig = (*Config)(nil)

// SourceRedeliversUnsettled implements ports.SourceRedeliveryConfig for a route
// whose receiver carries this config.
//
// It reads the session's EFFECTIVE state rather than the raw options, so it
// answers the same question the dialer will act on: an exclusive session is
// always resumed, an ephemeral one is always clean, and a persistent one follows
// its clean_start flag. A clean start is refused whatever the session expiry
// says, because the expiry governs how long the broker KEEPS a session and the
// clean-start flag discards it on the way in — the connect that matters is the
// one a restarted process makes, and it is an initial connect.
func (c *Config) SourceRedeliversUnsettled(
	session ports.SessionSpec,
	subscriptions []connectivity.SubscriptionPlan,
) (bool, string) {
	sessionCfg, err := configFromSpec(session.Config)
	if err != nil {
		return false, "the ingress session carries no MQTT configuration, so whether it keeps an " +
			"unacknowledged delivery cannot be established"
	}
	mode := session.SessionMode
	if mode == "" {
		mode = connectivity.SessionEphemeral
	}
	cleanStart, expiry := effectiveSessionState(sessionCfg.Session, mode)
	switch {
	case cleanStart:
		return false, fmt.Sprintf("the ingress session %q connects with clean_start, so a restarted "+
			"process is handed a fresh broker session and every delivery the old one held is gone "+
			"(set session_mode: persistent or exclusive with clean_start: false)", session.ID)
	case expiry == 0:
		return false, fmt.Sprintf("the ingress session %q is kept by the broker for zero seconds "+
			"after the connection closes, so a crash discards the deliveries it was holding "+
			"(set session_expiry_interval)", session.ID)
	}
	if len(subscriptions) == 0 {
		return false, "the receiver declares no subscriptions, so it has no delivery guarantee to " +
			"inherit from one"
	}
	for _, subscription := range subscriptions {
		if subscription.QoS < 1 {
			return false, fmt.Sprintf("subscription %q is QoS %d, which the broker delivers at most "+
				"once and never repeats (subscribe at QoS 1)", subscription.Topic, subscription.QoS)
		}
	}
	return true, ""
}

// BestEffortDirectHoldTopics admits QoS 0 without weakening the effective
// session requirements of any QoS 1/2 subscriptions on the same receiver.
func (c *Config) BestEffortDirectHoldTopics(
	session ports.SessionSpec,
	subscriptions []connectivity.SubscriptionPlan,
) ([]string, string) {
	sessionCfg, err := configFromSpec(session.Config)
	if err != nil {
		return nil, "the ingress session carries no MQTT configuration"
	}
	if err := sessionCfg.ValidateEffectiveSession(session.SessionMode); err != nil {
		return nil, fmt.Sprintf("the ingress session %q is invalid: %v", session.ID, err)
	}
	if len(subscriptions) == 0 {
		return nil, "the receiver declares no subscriptions"
	}
	var topics []string
	var strong []connectivity.SubscriptionPlan
	for _, subscription := range subscriptions {
		if err := ValidateMQTTSubscription(subscription.Topic, subscription.QoS); err != nil {
			return nil, fmt.Sprintf("invalid subscription: %v", err)
		}
		if subscription.QoS == 0 {
			topics = append(topics, subscription.Topic)
		} else {
			strong = append(strong, subscription)
		}
	}
	if len(strong) > 0 {
		if redelivers, refusal := c.SourceRedeliversUnsettled(session, strong); !redelivers {
			return nil, refusal
		}
	}
	return topics, ""
}

var _ ports.BestEffortDirectHoldConfig = (*Config)(nil)
