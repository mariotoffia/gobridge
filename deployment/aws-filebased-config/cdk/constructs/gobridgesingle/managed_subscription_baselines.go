package gobridgesingle

import (
	"fmt"
	"maps"
	"slices"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// A persistent or exclusive MQTT session owes the broker an exact record of the
// filters it installed, and does not start until that record's baseline exists
// — a missing baseline is "history unknown", not "no history" (ADR 0003). On
// this profile the store may live on the config mount, which only the task can
// write, so the attestation is stamped into the bootstrap document and the
// runtime seeds it at every boot. The facade requires it for every such
// session: a task that came up without one would report healthy and never
// subscribe.
//
// "Every such session" is exactly the set the runtime demands one for, which is
// every persistent or exclusive MQTT session once stores.managed_subscriptions
// is configured — subscriptions or not. A publish-only durable session is
// included deliberately: the store is what lets a replacement remove the
// filters a previous runtime installed under that identity, so the runtime
// requires its history too.

// managedSubscriptionBaselines validates the declared attestations against the
// bridge config and returns them deduplicated and sorted, so the bootstrap
// document is the same however the operator wrote them.
func managedSubscriptionBaselines(cfg *ports.BridgeConfig, declared map[string][]string) (map[string][]string, error) {
	durable := durableSessionsNeedingBaseline(cfg)
	for _, sessionID := range slices.Sorted(maps.Keys(declared)) {
		if !durable[sessionID] {
			return nil, fmt.Errorf("ManagedSubscriptionBaselines names session %q, which needs no baseline: it is "+
				"not a persistent or exclusive MQTT session, or this config configures no "+
				"stores.managed_subscriptions to keep one in", sessionID)
		}
	}
	out := make(map[string][]string, len(durable))
	for _, sessionID := range slices.Sorted(maps.Keys(durable)) {
		filters, ok := declared[sessionID]
		if !ok {
			return nil, fmt.Errorf("session %q is a persistent or exclusive MQTT session and does not start "+
				"without its managed-subscription baseline; set ManagedSubscriptionBaselines[%q] to the exact "+
				"filters its broker identity already holds, or to an empty list for a new identity",
				sessionID, sessionID)
		}
		seen := make(map[string]struct{}, len(filters))
		validated := make([]string, 0, len(filters))
		for _, filter := range filters {
			if filter == "" {
				return nil, fmt.Errorf("managed-subscription baseline for session %q contains an empty filter", sessionID)
			}
			if err := paho.ValidateMQTTTopicFilter(filter); err != nil {
				return nil, fmt.Errorf("managed-subscription baseline for session %q contains invalid filter %q: %w",
					sessionID, filter, err)
			}
			if _, duplicate := seen[filter]; duplicate {
				continue
			}
			seen[filter] = struct{}{}
			validated = append(validated, filter)
		}
		slices.Sort(validated)
		out[sessionID] = validated
	}
	return out, nil
}

// durableSessionsNeedingBaseline is the set of session ids the runtime requires
// a managed-subscription baseline for: MQTT sessions declared persistent or
// exclusive, once the config configures a store to keep one in. It mirrors the
// rule the bridge applies when it builds the session spec; the two must agree,
// or the deployment attests a set the runtime does not ask for and a session
// the runtime does ask for never starts.
func durableSessionsNeedingBaseline(cfg *ports.BridgeConfig) map[string]bool {
	durable := make(map[string]bool)
	if cfg == nil || cfg.Stores.ManagedSubscriptions == nil {
		return durable
	}
	for i := range cfg.Sessions {
		sd := &cfg.Sessions[i]
		mode := connectivity.SessionMode(sd.SessionMode)
		if (sd.Transport == paho.ShortKind || sd.Transport == paho.QualifiedKind) &&
			(mode == connectivity.SessionPersistent || mode == connectivity.SessionExclusive) {
			durable[sd.ID] = true
		}
	}
	return durable
}
