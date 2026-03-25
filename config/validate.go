package config

import (
	"fmt"
	"strings"
	"time"
)

// ValidationError collects multiple validation problems.
type ValidationError struct {
	Errors []string
}

func (e *ValidationError) Error() string {
	return "config validation failed:\n  " + strings.Join(e.Errors, "\n  ")
}

func (e *ValidationError) add(msg string) {
	e.Errors = append(e.Errors, msg)
}

func (e *ValidationError) addf(format string, args ...any) {
	e.Errors = append(e.Errors, fmt.Sprintf(format, args...))
}

func (e *ValidationError) hasErrors() bool {
	return len(e.Errors) > 0
}

// Validate performs structural validation on a BridgeConfig. It checks
// required fields, referential integrity between IDs, and valid enum
// values. It does not check transport-specific options.
func Validate(cfg *BridgeConfig) error {
	ve := &ValidationError{}

	if cfg.Bridge.ID == "" {
		ve.add("bridge.id is required")
	}

	if cfg.Bridge.DeploymentMode != "" {
		validateEnum(ve, "bridge.deployment_mode", cfg.Bridge.DeploymentMode,
			"standalone", "clustered")
	}

	if cw := cfg.ConfigWatch; cw != nil {
		if cw.Mode != "" {
			validateEnum(ve, "config_watch.mode", cw.Mode, "notify", "poll")
		}
		if cw.PollInterval != "" {
			if _, err := time.ParseDuration(cw.PollInterval); err != nil {
				ve.addf("config_watch.poll_interval: invalid duration %q: %v", cw.PollInterval, err)
			}
		}
		if cw.Debounce != "" {
			if _, err := time.ParseDuration(cw.Debounce); err != nil {
				ve.addf("config_watch.debounce: invalid duration %q: %v", cw.Debounce, err)
			}
		}
	}

	sessionIDs := collectIDs(ve, "sessions", len(cfg.Sessions), func(i int) (string, string) {
		s := cfg.Sessions[i]
		if s.Transport == "" {
			ve.addf("sessions[%d] (%s): transport is required", i, s.ID)
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
			ve.addf("receivers[%d] (%s): transport or session_id is required", i, r.ID)
		}
		if r.SessionID != "" {
			if _, ok := sessionIDs[r.SessionID]; !ok {
				ve.addf("receivers[%d] (%s): session_id %q not found in sessions", i, r.ID, r.SessionID)
			}
		}
		return r.ID, fmt.Sprintf("receivers[%d]", i)
	})

	senderIDs := collectIDs(ve, "senders", len(cfg.Senders), func(i int) (string, string) {
		s := cfg.Senders[i]
		if s.Transport == "" && s.SessionID == "" {
			ve.addf("senders[%d] (%s): transport or session_id is required", i, s.ID)
		}
		if s.SessionID != "" {
			if _, ok := sessionIDs[s.SessionID]; !ok {
				ve.addf("senders[%d] (%s): session_id %q not found in sessions", i, s.ID, s.SessionID)
			}
		}
		return s.ID, fmt.Sprintf("senders[%d]", i)
	})

	bindingIDs := collectIDs(ve, "bindings", len(cfg.Bindings), func(i int) (string, string) {
		b := cfg.Bindings[i]
		if b.SenderID == "" {
			ve.addf("bindings[%d] (%s): sender_id is required", i, b.ID)
		} else if _, ok := senderIDs[b.SenderID]; !ok {
			ve.addf("bindings[%d] (%s): sender_id %q not found in senders", i, b.ID, b.SenderID)
		}
		if b.SessionID != "" {
			if _, ok := sessionIDs[b.SessionID]; !ok {
				ve.addf("bindings[%d] (%s): session_id %q not found in sessions", i, b.ID, b.SessionID)
			}
		}
		if b.Address == "" {
			ve.addf("bindings[%d] (%s): address is required", i, b.ID)
		}
		return b.ID, fmt.Sprintf("bindings[%d]", i)
	})

	_ = collectIDs(ve, "routes", len(cfg.Routes), func(i int) (string, string) {
		r := cfg.Routes[i]
		if r.ReceiverID == "" {
			ve.addf("routes[%d] (%s): receiver_id is required", i, r.ID)
		} else if _, ok := receiverIDs[r.ReceiverID]; !ok {
			ve.addf("routes[%d] (%s): receiver_id %q not found in receivers", i, r.ID, r.ReceiverID)
		}

		if r.DeliveryMode != "" {
			validateEnum(ve, fmt.Sprintf("routes[%d].delivery_mode", i), r.DeliveryMode,
				"direct_hold", "shared_outbox")
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

		if len(r.Bindings) == 0 {
			ve.addf("routes[%d] (%s): at least one binding is required", i, r.ID)
		}
		for j, bid := range r.Bindings {
			if _, ok := bindingIDs[bid]; !ok {
				ve.addf("routes[%d] (%s): bindings[%d] %q not found in bindings", i, r.ID, j, bid)
			}
		}

		if r.Session != nil {
			prefix := fmt.Sprintf("routes[%d] (%s)", i, r.ID)
			if r.Session.SessionID == "" {
				ve.addf("%s: session.session_id is required", prefix)
			} else if _, ok := sessionIDs[r.Session.SessionID]; !ok {
				ve.addf("%s: session.session_id %q not found in sessions", prefix, r.Session.SessionID)
			}
			if r.Session.SenderID == "" {
				ve.addf("%s: session.sender_id is required", prefix)
			} else if _, ok := senderIDs[r.Session.SenderID]; !ok {
				ve.addf("%s: session.sender_id %q not found in senders", prefix, r.Session.SenderID)
			}
			validateDrainStrategy(ve, prefix, r.Session)
		}

		if r.DeliveryMode == "shared_outbox" {
			if cfg.Stores.Outbox == nil {
				ve.addf("routes[%d] (%s): shared_outbox requires stores.outbox to be configured", i, r.ID)
			}
			if r.Session != nil {
				sessDef, hasSess := sessionIDs[r.Session.SessionID]
				_ = sessDef
				if hasSess {
					for si, s := range cfg.Sessions {
						if s.ID == r.Session.SessionID && s.SessionMode == "exclusive" {
							if cfg.Stores.Lease == nil {
								ve.addf("routes[%d] (%s): exclusive session %q requires stores.lease to be configured",
									i, r.ID, cfg.Sessions[si].ID)
							}
							break
						}
					}
				}
			}
		}

		return r.ID, fmt.Sprintf("routes[%d]", i)
	})

	if ve.hasErrors() {
		return ve
	}
	return nil
}

func collectIDs(ve *ValidationError, section string, n int, fn func(i int) (id, label string)) map[string]struct{} {
	ids := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id, label := fn(i)
		if id == "" {
			ve.addf("%s: %s: id is required", section, label)
			continue
		}
		if _, dup := ids[id]; dup {
			ve.addf("%s: duplicate id %q", section, id)
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
	ve.addf("%s: invalid value %q, must be one of: %s", field, value, strings.Join(allowed, ", "))
}

func validateDrainStrategy(ve *ValidationError, prefix string, sess *RouteSessionDef) {
	ds := sess.DrainStrategy
	if ds == nil {
		return
	}

	if sess.DrainInterval != "" {
		ve.addf("%s: session.drain_strategy and session.drain_interval are mutually exclusive", prefix)
	}

	field := prefix + ": session.drain_strategy"

	switch ds.Type {
	case "fixed_poll":
		if ds.Interval != "" {
			if _, err := time.ParseDuration(ds.Interval); err != nil {
				ve.addf("%s: invalid interval %q: %v", field, ds.Interval, err)
			}
		}

	case "adaptive_backoff":
		var minD, maxD time.Duration
		if ds.MinInterval != "" {
			d, err := time.ParseDuration(ds.MinInterval)
			if err != nil {
				ve.addf("%s: invalid min_interval %q: %v", field, ds.MinInterval, err)
			}
			minD = d
		}
		if ds.MaxInterval != "" {
			d, err := time.ParseDuration(ds.MaxInterval)
			if err != nil {
				ve.addf("%s: invalid max_interval %q: %v", field, ds.MaxInterval, err)
			}
			maxD = d
		}
		if minD > 0 && maxD > 0 && maxD < minD {
			ve.addf("%s: max_interval (%s) must be >= min_interval (%s)", field, ds.MaxInterval, ds.MinInterval)
		}
		if ds.Multiplier != 0 && ds.Multiplier <= 1.0 {
			ve.addf("%s: multiplier must be > 1.0, got %v", field, ds.Multiplier)
		}

	default:
		ve.addf("%s: invalid type %q, must be one of: fixed_poll, adaptive_backoff", field, ds.Type)
	}
}
