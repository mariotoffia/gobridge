package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ValidationError is the shape the config package returns from
// Validate / ValidateWithWarnings. It is an alias for the
// transport-neutral ports.BlueprintValidationError so admin layers
// (httpapi) can inspect Warnings/Errors without importing the config
// parser package.
type ValidationError = ports.BlueprintValidationError

// Validate performs structural validation on a ports.BridgeConfig. It checks
// required fields, referential integrity between IDs, and valid enum
// values. It does not check transport-specific options.
func Validate(cfg *ports.BridgeConfig) error {
	ve := validate(cfg)
	if ve.HasErrors() {
		return ve
	}
	return nil
}

// ValidateWithWarnings performs Validate and returns any non-fatal
// warnings (e.g. direct_hold fencing advisory) even when validation
// passes. The caller should log warnings but not treat them as errors.
func ValidateWithWarnings(cfg *ports.BridgeConfig) (warnings []string, err error) {
	ve := validate(cfg)
	if ve.HasErrors() {
		return ve.Warnings, ve
	}
	return ve.Warnings, nil
}

func validate(cfg *ports.BridgeConfig) *ValidationError {
	ve := &ValidationError{}

	if cfg.Bridge.ID == "" {
		ve.Add("bridge.id is required")
	}

	if cfg.Bridge.DeploymentMode != "" {
		validateEnum(ve, "bridge.deployment_mode", cfg.Bridge.DeploymentMode,
			"standalone", "clustered")
	}

	if cfg.Bridge.ShutdownTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.ShutdownTimeout); err != nil {
			ve.Addf("bridge.shutdown_timeout: invalid duration %q: %v", cfg.Bridge.ShutdownTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.shutdown_timeout: must be positive, got %s", cfg.Bridge.ShutdownTimeout)
		}
	}
	if cfg.Bridge.DrainTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.DrainTimeout); err != nil {
			ve.Addf("bridge.drain_timeout: invalid duration %q: %v", cfg.Bridge.DrainTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.drain_timeout: must be positive, got %s", cfg.Bridge.DrainTimeout)
		}
	}
	var perRecordDrain, maxDrain time.Duration
	if cfg.Bridge.PerRecordDrainTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.PerRecordDrainTimeout); err != nil {
			ve.Addf("bridge.per_record_drain_timeout: invalid duration %q: %v", cfg.Bridge.PerRecordDrainTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.per_record_drain_timeout: must be positive, got %s", cfg.Bridge.PerRecordDrainTimeout)
		} else {
			perRecordDrain = d
		}
	}
	if cfg.Bridge.MaxDrainTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.MaxDrainTimeout); err != nil {
			ve.Addf("bridge.max_drain_timeout: invalid duration %q: %v", cfg.Bridge.MaxDrainTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.max_drain_timeout: must be positive, got %s", cfg.Bridge.MaxDrainTimeout)
		} else {
			maxDrain = d
		}
	}
	if perRecordDrain > 0 && maxDrain > 0 && maxDrain < perRecordDrain {
		ve.Addf("bridge.max_drain_timeout (%s) must be >= per_record_drain_timeout (%s)",
			cfg.Bridge.MaxDrainTimeout, cfg.Bridge.PerRecordDrainTimeout)
	}

	if cw := cfg.ConfigWatch; cw != nil {
		if cw.Mode != "" {
			validateEnum(ve, "config_watch.mode", cw.Mode, "notify", "poll")
		}
		if cw.PollInterval != "" {
			if d, err := time.ParseDuration(cw.PollInterval); err != nil {
				ve.Addf("config_watch.poll_interval: invalid duration %q: %v", cw.PollInterval, err)
			} else if d <= 0 {
				ve.Addf("config_watch.poll_interval: must be positive, got %s", cw.PollInterval)
			}
		}
		if cw.Debounce != "" {
			if d, err := time.ParseDuration(cw.Debounce); err != nil {
				ve.Addf("config_watch.debounce: invalid duration %q: %v", cw.Debounce, err)
			} else if d <= 0 {
				ve.Addf("config_watch.debounce: must be positive, got %s", cw.Debounce)
			}
		}
	}

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

	receiverIDs := collectIDs(ve, "receivers", len(cfg.Receivers), func(i int) (string, string) {
		r := cfg.Receivers[i]
		if r.Transport == "" && r.SessionID == "" {
			ve.Addf("receivers[%d] (%s): transport or session_id is required", i, r.ID)
		}
		if r.SessionID != "" {
			if _, ok := sessionIDs[r.SessionID]; !ok {
				ve.Addf("receivers[%d] (%s): session_id %q not found in sessions", i, r.ID, r.SessionID)
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
			validateSessionDurations(ve, prefix, r.Session)
			validateDrainStrategy(ve, prefix, r.Session)
		}

		if r.DeliveryMode == "shared_outbox" {
			if cfg.Stores.Outbox == nil {
				ve.Addf("routes[%d] (%s): shared_outbox requires stores.outbox to be configured", i, r.ID)
			}
			if r.Session != nil {
				_, hasSess := sessionIDs[r.Session.SessionID]
				if hasSess {
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

	validateStaleClaimDuration(ve, cfg)

	return ve
}

// validateClusteredMQTTSubscriptions checks that MQTT receivers in clustered
// mode use either an exclusive session (lease-based single subscriber) or
// $share/ topic prefixes (MQTT v5 shared subscriptions) to prevent N-fold
// message duplication across instances.
func validateClusteredMQTTSubscriptions(ve *ValidationError, cfg *ports.BridgeConfig) {
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

func collectIDs(ve *ValidationError, section string, n int, fn func(i int) (id, label string)) map[string]struct{} {
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

func validateEnum(ve *ValidationError, field, value string, allowed ...string) {
	for _, a := range allowed {
		if value == a {
			return
		}
	}
	ve.Addf("%s: invalid value %q, must be one of: %s", field, value, strings.Join(allowed, ", "))
}

// validateStaleClaimDuration warns when the outbox store's
// stale_claim_duration is explicitly set to a value much larger than the
// session step-down grace periods. A staleClaimAge that exceeds 2x the
// maximum StepDownGrace delays failover recovery without preventing
// duplicate sends.
func validateStaleClaimDuration(ve *ValidationError, cfg *ports.BridgeConfig) {
	if cfg.Stores.Outbox == nil {
		return
	}
	opts := rawMap(cfg.Stores.Outbox.Raw())
	if opts == nil {
		return
	}
	raw, ok := opts["stale_claim_duration"]
	if !ok {
		return
	}

	var stale time.Duration
	switch v := raw.(type) {
	case string:
		d, err := time.ParseDuration(v)
		if err != nil {
			ve.Addf("stores.outbox.options.stale_claim_duration: invalid duration %q: %v", v, err)
			return
		}
		stale = d
	case time.Duration:
		stale = v
	default:
		ve.Addf("stores.outbox.options.stale_claim_duration: must be a duration string (e.g. \"30s\"), got %T", raw)
		return
	}

	if stale <= 0 {
		ve.Addf("stores.outbox.options.stale_claim_duration: must be positive, got %v", stale)
		return
	}

	maxGrace := domain.DefaultStepDownGrace
	for _, r := range cfg.Routes {
		if r.Session == nil {
			continue
		}
		grace := domain.DefaultStepDownGrace
		if r.Session.StepDownGrace != "" {
			if d, err := time.ParseDuration(r.Session.StepDownGrace); err == nil {
				grace = d
			}
		}
		if grace > maxGrace {
			maxGrace = grace
		}
	}

	if stale > 2*maxGrace {
		ve.Warnf("stores.outbox.options.stale_claim_duration (%s) is more than 2x "+
			"the maximum step_down_grace (%s); this delays failover recovery "+
			"without reducing duplicate sends — consider a value closer to "+
			"step_down_grace + 15s (%s)",
			stale, maxGrace, maxGrace+15*time.Second)
	}
}

func validateSessionDurations(ve *ValidationError, prefix string, sess *ports.RouteSessionDef) {
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

func validateDrainStrategy(ve *ValidationError, prefix string, sess *ports.RouteSessionDef) {
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
