package validate

import (
	"fmt"
	"regexp"
	"strings"
	"time"

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
				// ADV-F1-P2: a session unions its receivers by SessionID
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

	if cfg.Bridge.DeploymentMode == "clustered" {
		validateClusteredMQTTSubscriptions(ve, cfg)
	}

	if !ve.HasErrors() && len(ve.Warnings) == 0 {
		return nil
	}
	return ve
}

// validateClusteredMQTTSubscriptions checks that MQTT receivers in clustered
// mode use either an exclusive session (lease-based single subscriber) or
// $share/ topic prefixes (MQTT v5 shared subscriptions) to prevent N-fold
// message duplication across instances.
func validateClusteredMQTTSubscriptions(ve *ports.BlueprintValidationError, cfg *ports.BridgeConfig) {
	sessionsByID := make(map[string]ports.SessionDef, len(cfg.Sessions))
	for _, s := range cfg.Sessions {
		sessionsByID[s.ID] = s
	}

	for i, r := range cfg.Receivers {
		transport := r.Transport
		var sessionMode string

		if r.SessionID != "" {
			if s, ok := sessionsByID[r.SessionID]; ok {
				if transport == "" {
					transport = s.Transport
				}
				sessionMode = s.SessionMode
			}
		}

		if !strings.EqualFold(transport, "mqtt") {
			continue
		}
		if sessionMode == "exclusive" {
			continue
		}

		prefix := fmt.Sprintf("receivers[%d] (%s)", i, r.ID)
		for j, topic := range r.Topics {
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

// validateSessionDurationFields checks the foreign-key-adjacent
// duration fields on a route's session block. Pure field-level
// duration parsing for top-level sections (bridge.*, config_watch.*)
// remains in the config package.
func validateSessionDurationFields(ve *ports.BlueprintValidationError, prefix string, sess *ports.RouteSessionDef) {
	for _, f := range []struct{ name, val string }{
		{"lease_ttl", sess.LeaseTTL},
		{"renew_interval", sess.RenewInterval},
		{"step_down_grace", sess.StepDownGrace},
	} {
		if f.val == "" {
			continue
		}
		d, err := time.ParseDuration(f.val)
		if err != nil {
			ve.Addf("%s: session.%s: invalid duration %q: %v", prefix, f.name, f.val, err)
		} else if d <= 0 {
			ve.Addf("%s: session.%s: must be positive, got %s", prefix, f.name, f.val)
		}
	}
	if sess.MaxRenewFails < 0 {
		ve.Addf("%s: session.max_renew_fails: must be non-negative, got %d", prefix, sess.MaxRenewFails)
	}
}

func validateRouteDrainStrategy(ve *ports.BlueprintValidationError, prefix string, sess *ports.RouteSessionDef) {
	ds := sess.DrainStrategy
	if ds == nil {
		return
	}

	if sess.DrainInterval != "" {
		ve.Addf("%s: session.drain_strategy and session.drain_interval are mutually exclusive", prefix)
	}

	field := prefix + ": session.drain_strategy"

	switch ds.Type {
	case "fixed_poll":
		if ds.Interval != "" {
			if d, err := time.ParseDuration(ds.Interval); err != nil {
				ve.Addf("%s: invalid interval %q: %v", field, ds.Interval, err)
			} else if d <= 0 {
				ve.Addf("%s: interval must be positive, got %s", field, ds.Interval)
			}
		}

	case "adaptive_backoff":
		var minD, maxD time.Duration
		if ds.MinInterval != "" {
			d, err := time.ParseDuration(ds.MinInterval)
			if err != nil {
				ve.Addf("%s: invalid min_interval %q: %v", field, ds.MinInterval, err)
			}
			minD = d
		}
		if ds.MaxInterval != "" {
			d, err := time.ParseDuration(ds.MaxInterval)
			if err != nil {
				ve.Addf("%s: invalid max_interval %q: %v", field, ds.MaxInterval, err)
			}
			maxD = d
		}
		if minD > 0 && maxD > 0 && maxD < minD {
			ve.Addf("%s: max_interval (%s) must be >= min_interval (%s)", field, ds.MaxInterval, ds.MinInterval)
		}
		if ds.Multiplier != 0 && ds.Multiplier <= 1.0 {
			ve.Addf("%s: multiplier must be > 1.0, got %v", field, ds.Multiplier)
		}

	default:
		ve.Addf("%s: invalid type %q, must be one of: fixed_poll, adaptive_backoff", field, ds.Type)
	}
}

func validateResolver(ve *ports.BlueprintValidationError, prefix string, r ports.RouteDef) {
	res := r.Resolver
	if !validResolverTypes[res.Type] {
		ve.Addf("%s: resolver.type %q is invalid; must be one of: rules, header_map, all, static", prefix, res.Type)
		return
	}

	bindingSet := make(map[string]bool, len(r.Bindings))
	for _, bid := range r.Bindings {
		bindingSet[bid] = true
	}

	if res.DefaultBinding != "" && !bindingSet[res.DefaultBinding] {
		ve.Addf("%s: resolver.default_binding %q not found in route bindings", prefix, res.DefaultBinding)
	}

	switch res.Type {
	case "header_map":
		validateHeaderMapResolver(ve, prefix, res, bindingSet)
	case "rules":
		validateRulesResolver(ve, prefix, res, bindingSet)
	}
}

func validateHeaderMapResolver(ve *ports.BlueprintValidationError, prefix string, res *ports.ResolverDef, bindingSet map[string]bool) {
	if res.HeaderKey == "" {
		ve.Addf("%s: resolver.header_key is required for header_map type", prefix)
	}
	if len(res.HeaderMap) == 0 {
		ve.Addf("%s: resolver.header_map must have at least one entry", prefix)
	}
	for val, bid := range res.HeaderMap {
		if !bindingSet[bid] {
			ve.Addf("%s: resolver.header_map[%q] references unknown binding %q", prefix, val, bid)
		}
	}
}

func validateRulesResolver(ve *ports.BlueprintValidationError, prefix string, res *ports.ResolverDef, bindingSet map[string]bool) {
	if len(res.Rules) == 0 && res.DefaultBinding == "" {
		ve.Addf("%s: resolver type \"rules\" requires at least one rule or a default_binding", prefix)
	}
	for i, rule := range res.Rules {
		rp := prefix + ".resolver.rules[" + itoa(i) + "]"
		if rule.BindingID == "" {
			ve.Addf("%s: binding_id is required", rp)
		} else if !bindingSet[rule.BindingID] {
			ve.Addf("%s: binding_id %q not found in route bindings", rp, rule.BindingID)
		}
		for j, cond := range rule.Match {
			cp := rp + ".match[" + itoa(j) + "]"
			if cond.Field == "" {
				ve.Addf("%s: field is required", cp)
			}
			if cond.Operator == "" {
				ve.Addf("%s: operator is required", cp)
			} else if !validConditionOperators[cond.Operator] {
				ve.Addf("%s: operator %q is invalid", cp, cond.Operator)
			}
			if cond.Operator == "regex" {
				if pattern, ok := cond.Value.(string); ok {
					if len(pattern) > MaxRegexPatternLen {
						ve.Addf("%s: regex pattern exceeds maximum length of %d characters", cp, MaxRegexPatternLen)
					} else if _, err := regexp.Compile(pattern); err != nil {
						ve.Addf("%s: invalid regex pattern: %v", cp, err)
					}
				} else {
					ve.Addf("%s: regex operator requires a string value", cp)
				}
			}
		}
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return string(digits[i])
	}
	buf := make([]byte, 0, 4)
	for i > 0 {
		buf = append(buf, digits[i%10])
		i /= 10
	}
	for l, r := 0, len(buf)-1; l < r; l, r = l+1, r-1 {
		buf[l], buf[r] = buf[r], buf[l]
	}
	return string(buf)
}
