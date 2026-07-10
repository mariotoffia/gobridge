package paho

import "strings"

// matchTopicFilter reports whether an incoming publish topic name matches
// an MQTT topic filter, per MQTT v5 §4.7 (Topic Names and Topic Filters):
//
//   - "+" matches exactly one topic level.
//   - "#" matches any number of levels (including zero) and must be the
//     last level of the filter.
//   - A shared-subscription filter ("$share/<group>/<filter>") matches
//     against its embedded <filter> — the broker delivers on the real
//     topic name, never on the "$share/..." string.
//   - Topics beginning with "$" (e.g. "$SYS/...") are NOT matched by
//     filters whose first level is a wildcard ("+" or "#").
//
// The router (acl_router.go) uses this to dispatch each publish only to
// the Receivers whose subscription filters cover it — a shared Session
// with multiple Receivers no longer fans every message out to every
// handler (cross-receiver fan-out finding).
func matchTopicFilter(filter, topic string) bool {
	filter = stripSharedSubscriptionPrefix(filter)
	if filter == "" {
		return false
	}

	// MQTT v5 §4.7.2: topics beginning with '$' must not be matched by
	// filters starting with a wildcard.
	if strings.HasPrefix(topic, "$") &&
		(strings.HasPrefix(filter, "+") || strings.HasPrefix(filter, "#")) {
		return false
	}

	fLevels := strings.Split(filter, "/")
	tLevels := strings.Split(topic, "/")

	for i, fl := range fLevels {
		if fl == "#" {
			// '#' must be the last filter level; it matches the parent
			// level and any number of child levels.
			return i == len(fLevels)-1
		}
		if i >= len(tLevels) {
			return false
		}
		if fl == "+" {
			continue
		}
		if fl != tLevels[i] {
			return false
		}
	}
	return len(fLevels) == len(tLevels)
}

// matchesAnyFilter reports whether topic matches at least one filter in
// filters. An EMPTY filter list matches everything — a Receiver created
// without subscription topics (legacy / test construction) keeps the
// historical receive-all behaviour.
func matchesAnyFilter(filters []string, topic string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if matchTopicFilter(f, topic) {
			return true
		}
	}
	return false
}

// stripSharedSubscriptionPrefix removes a leading "$share/<group>/" from
// a topic filter so shared subscriptions match the topic names the
// broker actually delivers on. Malformed shared filters (missing group
// or missing embedded filter) are returned unchanged.
func stripSharedSubscriptionPrefix(filter string) string {
	const prefix = "$share/"
	if !strings.HasPrefix(filter, prefix) {
		return filter
	}
	rest := filter[len(prefix):]
	idx := strings.IndexByte(rest, '/')
	if idx <= 0 || idx == len(rest)-1 {
		return filter
	}
	return rest[idx+1:]
}

// isSharedSubscriptionFilter reports whether a subscription filter is a shared
// subscription ("$share/<group>/<filter>"). A shared subscription makes the
// broker LOAD-BALANCE a topic's deliveries across the group's members, which
// is the horizontal scale-out path — and the one that REQUIRES a UNIQUE
// per-instance client_id (HIGH-3): replicas that reuse a client_id form a
// SINGLE broker session and take each other over (self-DOS) instead of sharing
// the load. Any "$share/" prefix — even a malformed one — signals that
// scale-out intent, so detection is intentionally broader than the strict
// stripSharedSubscriptionPrefix parse.
func isSharedSubscriptionFilter(filter string) bool {
	return strings.HasPrefix(filter, "$share/")
}
