package validate

import (
	"fmt"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// MaxRegexPatternLen is the maximum allowed length for a regex pattern
// in a resolver rule condition. Rejecting overly large patterns guards
// against regex DoS / pathological compile times.
const MaxRegexPatternLen = 4096

var validResolverTypes = map[string]bool{
	"rules":      true,
	"header_map": true,
	"all":        true,
	"static":     true,
}

var validConditionOperators = map[string]bool{
	"eq": true, "ne": true, "prefix": true, "contains": true,
	"regex": true, "gt": true, "lt": true, "gte": true, "lte": true,
	"exists": true, "in": true,
}

// ValidateBlueprintGraph performs cross-reference validation of the
// route graph in a parsed bridge blueprint: session, receiver, sender,
// binding, and route IDs must be unique within their section, every
// foreign-key reference must resolve, resolver rules and header maps
// must point at valid bindings, and clustered MQTT receivers must use
// either an exclusive session or a $share/ topic prefix to avoid
// N-fold message duplication.
//
// It returns nil when the graph is consistent, otherwise a
// *ports.BlueprintValidationError carrying every detected error and
// any non-fatal warnings (e.g. direct_hold fencing advisories).
//
// The function performs no field-level structural validation
// (required fields, enum values, duration parsing); those checks
// remain in the config package and are merged with the result of
// this function by config.Validate.
func ValidateBlueprintGraph(cfg *ports.BridgeConfig) *ports.BlueprintValidationError {
	ve := &ports.BlueprintValidationError{}

	sessionIDs := collectIDs(ve, "sessions", len(cfg.Sessions), func(i int) (string, string) {
		s := cfg.Sessions[i]
		if s.Transport == "" {
			ve.Addf("sessions[%d] (%s): transport is required", i, s.ID)
		}
		if s.SessionMode != "" {
			validateEnum(ve, fmt.Sprintf("sessions[%d].session_mode", i), s.SessionMode,
				"ephemeral", "persistent", "exclusive")
		}
		return s.ID, fmt.Sprintf("sessions[%d]", i)
	})

	sessionsByID := make(map[string]ports.SessionDef, len(cfg.Sessions))
	for _, s := range cfg.Sessions {
		sessionsByID[s.ID] = s
	}

	receiverIDs := collectIDs(ve, "receivers", len(cfg.Receivers), func(i int) (string, string) {
		r := cfg.Receivers[i]
		if r.Transport == "" && r.SessionID == "" {
			ve.Addf("receivers[%d] (%s): transport or session_id is required", i, r.ID)
		}
		if r.SessionID != "" {
			if _, ok := sessionIDs[r.SessionID]; !ok {
				ve.Addf("receivers[%d] (%s): session_id %q not found in sessions", i, r.ID, r.SessionID)
			} else if sess := sessionsByID[r.SessionID]; r.Transport != "" && sess.Transport != "" && r.Transport != sess.Transport {
				// a session unions its receivers by SessionID
				// regardless of transport; only fire when both transports
				// are explicitly set and differ, so there is no false positive.
				ve.Addf("receivers[%d] (%s): receiver transport %q does not match session %q transport %q: "+
					"a session and its receivers must share one transport",
					i, r.ID, r.Transport, r.SessionID, sess.Transport)
			}
		}
		return r.ID, fmt.Sprintf("receivers[%d]", i)
	})

	senderIDs := collectIDs(ve, "senders", len(cfg.Senders), func(i int) (string, string) {
		s := cfg.Senders[i]
		if s.Transport == "" && s.SessionID == "" {
			ve.Addf("senders[%d] (%s): transport or session_id is required", i, s.ID)
		}
		if s.SessionID != "" {
			if _, ok := sessionIDs[s.SessionID]; !ok {
				ve.Addf("senders[%d] (%s): session_id %q not found in sessions", i, s.ID, s.SessionID)
			}
		}
		return s.ID, fmt.Sprintf("senders[%d]", i)
	})

	bindingIDs := collectIDs(ve, "bindings", len(cfg.Bindings), func(i int) (string, string) {
		b := cfg.Bindings[i]
		if b.SenderID == "" {
			ve.Addf("bindings[%d] (%s): sender_id is required", i, b.ID)
		} else if _, ok := senderIDs[b.SenderID]; !ok {
			ve.Addf("bindings[%d] (%s): sender_id %q not found in senders", i, b.ID, b.SenderID)
		}
		if b.SessionID != "" {
			if _, ok := sessionIDs[b.SessionID]; !ok {
				ve.Addf("bindings[%d] (%s): session_id %q not found in sessions", i, b.ID, b.SessionID)
			}
		}
		if b.Address == "" {
			ve.Addf("bindings[%d] (%s): address is required", i, b.ID)
		}
		return b.ID, fmt.Sprintf("bindings[%d]", i)
	})

	_ = collectIDs(ve, "routes", len(cfg.Routes), func(i int) (string, string) {
		r := cfg.Routes[i]
		if r.ReceiverID == "" {
			ve.Addf("routes[%d] (%s): receiver_id is required", i, r.ID)
		} else if _, ok := receiverIDs[r.ReceiverID]; !ok {
			ve.Addf("routes[%d] (%s): receiver_id %q not found in receivers", i, r.ID, r.ReceiverID)
		}

		if r.DeliveryMode != "" {
			validateEnum(ve, fmt.Sprintf("routes[%d].delivery_mode", i), r.DeliveryMode,
				"direct_hold", "shared_outbox")
		}
		if r.DeliveryMode == "direct_hold" || r.DeliveryMode == "" {
			ve.Warnf("routes[%d] (%s): direct_hold mode provides no inter-instance fencing; "+
				"multiple instances will send independently — destination must handle duplicates idempotently",
				i, r.ID)
		}
		if r.DispatchMode != "" {
			validateEnum(ve, fmt.Sprintf("routes[%d].dispatch_mode", i), r.DispatchMode,
				"single", "fan_out")
		}

		if r.Policy.AckAfter != "" {
			validateEnum(ve, fmt.Sprintf("routes[%d].policy.ack_after", i), r.Policy.AckAfter,
				"target_accept", "outbox_persist")
		}
		if r.Policy.OnExpired != "" {
			validateEnum(ve, fmt.Sprintf("routes[%d].policy.on_expired", i), r.Policy.OnExpired,
				"drop", "dlq")
		}
		if r.Policy.OnPermanentFailure != "" {
			validateEnum(ve, fmt.Sprintf("routes[%d].policy.on_permanent_failure", i), r.Policy.OnPermanentFailure,
				"drop", "dlq")
		}
		if r.Policy.OnFiltered != "" {
			validateEnum(ve, fmt.Sprintf("routes[%d].policy.on_filtered", i), r.Policy.OnFiltered,
				"drop", "dlq")
		}
		if r.Policy.SendTimeout != "" {
			if d, err := time.ParseDuration(r.Policy.SendTimeout); err != nil {
				ve.Addf("routes[%d].policy.send_timeout: invalid duration %q: %v", i, r.Policy.SendTimeout, err)
			} else if d <= 0 {
				ve.Addf("routes[%d].policy.send_timeout: must be positive, got %s", i, r.Policy.SendTimeout)
			}
		}
		if r.Policy.DepthCacheTTL != "" {
			if d, err := time.ParseDuration(r.Policy.DepthCacheTTL); err != nil {
				ve.Addf("routes[%d].policy.depth_cache_ttl: invalid duration %q: %v", i, r.Policy.DepthCacheTTL, err)
			} else if d <= 0 {
				ve.Addf("routes[%d].policy.depth_cache_ttl: must be positive, got %s", i, r.Policy.DepthCacheTTL)
			}
		}
		validateRoutePolicyBuildFields(ve, i, r.Policy)

		if len(r.Bindings) == 0 {
			ve.Addf("routes[%d] (%s): at least one binding is required", i, r.ID)
		}
		for j, bid := range r.Bindings {
			if _, ok := bindingIDs[bid]; !ok {
				ve.Addf("routes[%d] (%s): bindings[%d] %q not found in bindings", i, r.ID, j, bid)
			}
		}

		if r.Session != nil {
			prefix := fmt.Sprintf("routes[%d] (%s)", i, r.ID)
			if r.Session.SessionID == "" {
				ve.Addf("%s: session.session_id is required", prefix)
			} else if _, ok := sessionIDs[r.Session.SessionID]; !ok {
				ve.Addf("%s: session.session_id %q not found in sessions", prefix, r.Session.SessionID)
			}
			if r.Session.SenderID == "" {
				ve.Addf("%s: session.sender_id is required", prefix)
			} else if _, ok := senderIDs[r.Session.SenderID]; !ok {
				ve.Addf("%s: session.sender_id %q not found in senders", prefix, r.Session.SenderID)
			}
			validateSessionDurationFields(ve, prefix, r.Session)
			validateSessionLeaseCadence(ve, prefix, r.Session, ports.IsClusteredDeployment(cfg))
			validateRouteDrainStrategy(ve, prefix, r.Session)
		}

		if r.DeliveryMode == "shared_outbox" {
			if cfg.Stores.Outbox == nil {
				ve.Addf("routes[%d] (%s): shared_outbox requires stores.outbox to be configured", i, r.ID)
			}
			if r.Session != nil {
				if _, hasSess := sessionIDs[r.Session.SessionID]; hasSess {
					for si, s := range cfg.Sessions {
						if s.ID == r.Session.SessionID && s.SessionMode == "exclusive" {
							if cfg.Stores.Lease == nil {
								ve.Addf("routes[%d] (%s): exclusive session %q requires stores.lease to be configured",
									i, r.ID, cfg.Sessions[si].ID)
							}
							break
						}
					}
				}
			}
		}

		if r.Resolver != nil {
			validateResolver(ve, fmt.Sprintf("routes[%d] (%s)", i, r.ID), r)
		}

		return r.ID, fmt.Sprintf("routes[%d]", i)
	})

	// CLUSTER-1: use the single canonical clustered predicate (mode OR static
	// endpoints) so a static-endpoints deployment cannot activate clustered
	// runtime behavior while bypassing these replica-safety checks.
	if ports.IsClusteredDeployment(cfg) {
		validateClusteredMQTTSubscriptions(ve, cfg)
	}

	validateSameSessionRouteLoop(ve, cfg)

	if !ve.HasErrors() && len(ve.Warnings) == 0 {
		return nil
	}
	return ve
}

// validateSameSessionRouteLoop warns when a binding publishes to an address
// that a receiver on the SAME session subscribes to. On a single broker
// (session) this is a static feedback loop: every delivered message is
// re-consumed on that subscription and re-sent, indefinitely. Only exact
// literal matches are flagged (the static case the finding scopes to);
// templated addresses (e.g. "a/{id}") and wildcard subscriptions do not match
// and never produce a warning, so false positives are avoided. It is a warning,
// not an error, because a topology may legitimately bounce through an external
// system that rewrites the message before it re-enters.
func validateSameSessionRouteLoop(ve *ports.BlueprintValidationError, cfg *ports.BridgeConfig) {
	if len(cfg.Bindings) == 0 || len(cfg.Receivers) == 0 {
		return
	}

	senderSession := make(map[string]string, len(cfg.Senders))
	for _, s := range cfg.Senders {
		senderSession[s.ID] = s.SessionID
	}

	// session id -> subscribed topic -> first receiver id declaring it.
	topicsBySession := make(map[string]map[string]string)
	for _, r := range cfg.Receivers {
		if r.SessionID == "" {
			continue
		}
		for _, t := range r.Topics {
			if t.Topic == "" {
				continue
			}
			topics := topicsBySession[r.SessionID]
			if topics == nil {
				topics = make(map[string]string)
				topicsBySession[r.SessionID] = topics
			}
			if _, exists := topics[t.Topic]; !exists {
				topics[t.Topic] = r.ID
			}
		}
	}
	if len(topicsBySession) == 0 {
		return
	}

	for i, b := range cfg.Bindings {
		if b.Address == "" {
			continue
		}
		// A binding's effective session is its own session_id, or the session
		// of the sender it publishes through.
		sess := b.SessionID
		if sess == "" {
			sess = senderSession[b.SenderID]
		}
		if sess == "" {
			continue
		}
		if rxID, ok := topicsBySession[sess][b.Address]; ok {
			ve.Warnf("bindings[%d] (%s): address %q on session %q is also a subscription topic of "+
				"receiver %q on the same session; messages sent here are re-consumed and re-sent "+
				"(feedback loop) — publish to a distinct address/topic or route through a separate "+
				"session/broker", i, b.ID, b.Address, sess, rxID)
		}
	}
}

// validateClusteredMQTTSubscriptions validates clustered shared-consumer
// configs through optional typed plugin capabilities. It deliberately has no
// transport-name switch: adapters declare whether they can prove a per-replica
// identity, while the transport-neutral validator recognizes shared-subscription
// syntax and fails closed when the capability is unavailable.
func validateClusteredMQTTSubscriptions(ve *ports.BlueprintValidationError, cfg *ports.BridgeConfig) {
	sessionsByID := make(map[string]ports.SessionDef, len(cfg.Sessions))
	for _, session := range cfg.Sessions {
		sessionsByID[session.ID] = session
	}

	for i, receiver := range cfg.Receivers {
		var (
			sessionMode  string
			pluginConfig ports.PluginConfig
		)
		if receiver.SessionID != "" {
			if session, ok := sessionsByID[receiver.SessionID]; ok {
				sessionMode = session.SessionMode
				pluginConfig = session.Config
			}
		}
		if pluginConfig == nil {
			pluginConfig = receiver.Config
		}

		prefix := fmt.Sprintf("receivers[%d] (%s)", i, receiver.ID)
		replicaConfig, capabilityAvailable := pluginConfig.(ports.ReplicaIdentityConfig)
		hasSharedTopic := false
		for _, topic := range receiver.Topics {
			if isSharedTopic(topic.Topic) {
				hasSharedTopic = true
				break
			}
		}

		// A plugin advertising replica identity is a shared-consumer transport,
		// so retain the existing requirement that every clustered non-exclusive
		// subscription use shared syntax. A literal $share topic also enters this
		// path and fails closed if its plugin capability is absent.
		if !capabilityAvailable && !hasSharedTopic {
			continue
		}

		strategy := ""
		if capabilityAvailable {
			if ports.IsNilPluginConfig(pluginConfig) {
				ve.Addf("%s: replica identity config is nil", prefix)
				continue
			}
			strategy = replicaConfig.ReplicaIdentityStrategy()
		}
		if sessionMode == string(connectivity.SessionExclusive) {
			if strategy != "" {
				ve.Addf("%s: exclusive session must not configure a replica identity strategy; "+
					"exclusive ownership requires one stable client identity", prefix)
			}
			continue
		}

		for j, topic := range receiver.Topics {
			if !isSharedTopic(topic.Topic) {
				ve.Addf("%s: topics[%d]: clustered MQTT receiver requires $share/ topic prefix "+
					"or exclusive session to prevent N-fold message duplication; got %q",
					prefix, j, topic.Topic)
			} else if !isValidSharedTopic(topic.Topic) {
				ve.Addf("%s: topics[%d]: malformed $share/ topic %q: "+
					"must be $share/<group>/<topic> with non-empty group and topic",
					prefix, j, topic.Topic)
			}
		}

		switch strategy {
		case ports.ReplicaIdentityHostname:
			// Stable and deployment-verifiable; preferred for all shared modes.
		case ports.ReplicaIdentityNonce:
			if sessionMode != string(connectivity.SessionEphemeral) && sessionMode != "" {
				ve.Addf("%s: replica identity strategy %q is valid only for ephemeral shared sessions; "+
					"use %q for durable sessions", prefix, strategy, ports.ReplicaIdentityHostname)
			}
		case "":
			ve.Addf("%s: clustered shared subscription requires a verifiable replica identity strategy; "+
				"configure %q (preferred), or %q only for ephemeral sessions",
				prefix, ports.ReplicaIdentityHostname, ports.ReplicaIdentityNonce)
		default:
			ve.Addf("%s: replica identity strategy %q is not verifiable; use %q, or %q only for ephemeral sessions",
				prefix, strategy, ports.ReplicaIdentityHostname, ports.ReplicaIdentityNonce)
		}
	}
}

func isSharedTopic(topic string) bool {
	return strings.HasPrefix(topic, "$share/")
}

func isValidSharedTopic(topic string) bool {
	rest := topic[len("$share/"):]
	slashIdx := strings.Index(rest, "/")
	return slashIdx > 0 && slashIdx < len(rest)-1
}

func validateEnum(ve *ports.BlueprintValidationError, field, value string, allowed ...string) {
	for _, a := range allowed {
		if value == a {
			return
		}
	}
	ve.Addf("%s: invalid value %q, must be one of: %s", field, value, strings.Join(allowed, ", "))
}

func collectIDs(ve *ports.BlueprintValidationError, section string, n int, fn func(i int) (id, label string)) map[string]struct{} {
	ids := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id, label := fn(i)
		if id == "" {
			ve.Addf("%s: %s: id is required", section, label)
			continue
		}
		if _, dup := ids[id]; dup {
			ve.Addf("%s: duplicate id %q", section, id)
		}
		ids[id] = struct{}{}
	}
	return ids
}

// validateRoutePolicyBuildFields validates the route-policy fields that
// bridge/convert.go (toRoutePolicyE) parses at BUILD time — replay_budget and
// the retry backoff block — so a bad value fails validation instead of passing
// the config transaction and durable write only to fail at apply/restart
// (finding: validation misses fields parsed later at build). The checks mirror
// the builder exactly: replay_budget must be a non-negative duration, the
// backoff intervals must be parseable durations, and backoff.jitter must be in
// [0,1]. send_timeout and depth_cache_ttl are validated inline at the call site.
func validateRoutePolicyBuildFields(ve *ports.BlueprintValidationError, i int, p ports.PolicyDef) {
	if p.ReplayBudget != "" {
		if d, err := time.ParseDuration(p.ReplayBudget); err != nil {
			ve.Addf("routes[%d].policy.replay_budget: invalid duration %q: %v", i, p.ReplayBudget, err)
		} else if d < 0 {
			ve.Addf("routes[%d].policy.replay_budget: must not be negative, got %s", i, p.ReplayBudget)
		}
	}
	// time.ParseDuration accepts a leading '-', and a negative retry interval is
	// nonsensical in both directions: a negative max_interval never clamps
	// (route.retryDelay gates the clamp on `> 0`), so the exponential grows to
	// +Inf and feeds Retry a near-infinite or negative delay.
	for _, f := range []struct{ name, val string }{
		{"initial_interval", p.Backoff.InitialInterval},
		{"max_interval", p.Backoff.MaxInterval},
	} {
		if f.val == "" {
			continue
		}
		if d, err := time.ParseDuration(f.val); err != nil {
			ve.Addf("routes[%d].policy.backoff.%s: invalid duration %q: %v", i, f.name, f.val, err)
		} else if d < 0 {
			ve.Addf("routes[%d].policy.backoff.%s: must not be negative, got %s", i, f.name, f.val)
		}
	}
	// A multiplier in (0,1) is decaying, not gentle, backoff: each retry fires
	// SOONER than the last, so a failing target is hammered hardest exactly when
	// it is least able to recover. Exactly 1 is a legal fixed retry interval.
	if p.Backoff.Multiplier != 0 && p.Backoff.Multiplier < 1 {
		ve.Addf("routes[%d].policy.backoff.multiplier: must be >= 1 (below 1 accelerates retries instead of backing off), got %v",
			i, p.Backoff.Multiplier)
	}
	// Omitted jitter takes the recommended default; an explicit 0 opts out.
	// Both are legal, so only a present, out-of-range fraction is rejected.
	if p.Backoff.Jitter != nil && (*p.Backoff.Jitter < 0 || *p.Backoff.Jitter > 1) {
		ve.Addf("routes[%d].policy.backoff.jitter: must be in [0,1], got %v", i, *p.Backoff.Jitter)
	}
}
