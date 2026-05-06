package validate

import (
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

func validateStructural(r *RouteConfig, cfg *BridgeConfig, errs *ValidationErrors) {
	if r.ID == "" {
		*errs = append(*errs, ValidationError{
			Rule:    "structural",
			Message: "route must have a non-empty ID",
		})
	}

	if len(r.Bindings) == 0 {
		*errs = append(*errs, ValidationError{
			RouteID: r.ID,
			Rule:    "structural",
			Message: "route must have at least one binding",
		})
	}

	if r.Policy.DeliveryMode == "" {
		*errs = append(*errs, ValidationError{
			RouteID: r.ID,
			Rule:    "structural",
			Message: "route must specify a delivery mode",
		})
	} else if r.Policy.DeliveryMode != routing.DeliveryDirectHold &&
		r.Policy.DeliveryMode != routing.DeliverySharedOutbox {
		*errs = append(*errs, ValidationError{
			RouteID: r.ID,
			Rule:    "structural",
			Message: fmt.Sprintf("unrecognized delivery mode %q", r.Policy.DeliveryMode),
		})
	}

	if r.Policy.DispatchMode != "" &&
		r.Policy.DispatchMode != routing.DispatchSingle &&
		r.Policy.DispatchMode != routing.DispatchFanOut {
		*errs = append(*errs, ValidationError{
			RouteID: r.ID,
			Rule:    "structural",
			Message: fmt.Sprintf("unrecognized dispatch mode %q", r.Policy.DispatchMode),
		})
	}

	for _, b := range r.Bindings {
		if b.SessionID == "" {
			continue
		}
		if _, ok := cfg.Sessions[b.SessionID]; !ok {
			*errs = append(*errs, ValidationError{
				RouteID: r.ID,
				Rule:    "structural",
				Message: fmt.Sprintf("binding %q references unknown session %q", b.ID, b.SessionID),
			})
		}
	}
}

func validateDirectHold(r *RouteConfig, cfg *BridgeConfig, errs *ValidationErrors) {
	if !r.HasCapability(ports.CapVisibilityExtension) {
		*errs = append(*errs, ValidationError{
			RouteID: r.ID,
			Rule:    "direct_hold",
			Message: "direct_hold invalid: source does not support visibility extension",
		})
	}

	if r.Policy.DispatchMode == routing.DispatchFanOut {
		*errs = append(*errs, ValidationError{
			RouteID: r.ID,
			Rule:    "direct_hold",
			Message: "direct_hold invalid: resolver fan-out is enabled",
		})
	}

	for _, b := range r.Bindings {
		if b.SessionID == "" {
			continue
		}
		if sess, ok := cfg.Sessions[b.SessionID]; ok && sess.Mode == connectivity.SessionExclusive {
			*errs = append(*errs, ValidationError{
				RouteID: r.ID,
				Rule:    "direct_hold",
				Message: "direct_hold invalid: target session requires lease handoff",
			})
			break
		}
	}
}

func validateSharedOutbox(r *RouteConfig, cfg *BridgeConfig, errs *ValidationErrors) {
	if !cfg.HasOutboxStore {
		*errs = append(*errs, ValidationError{
			RouteID: r.ID,
			Rule:    "shared_outbox",
			Message: "shared_outbox invalid: no OutboxStore configured",
		})
	}

	for _, b := range r.Bindings {
		if b.SessionID == "" {
			continue
		}
		if sess, ok := cfg.Sessions[b.SessionID]; ok && sess.Mode == connectivity.SessionExclusive {
			if !cfg.HasLeaseStore {
				*errs = append(*errs, ValidationError{
					RouteID: r.ID,
					Rule:    "shared_outbox",
					Message: "shared_outbox invalid: no LeaseStore configured for exclusive session",
				})
			}
			break
		}
	}

	if !r.HasIdempotencyProc && !r.SourceGuaranteesID {
		*errs = append(*errs, ValidationError{
			RouteID: r.ID,
			Rule:    "shared_outbox",
			Message: "shared_outbox invalid: no idempotency key processor configured and source does not guarantee Envelope.ID",
		})
	}

	txLimit := cfg.OutboxTransactionLimit
	if txLimit <= 0 {
		txLimit = DefaultOutboxTransactionLimit
	}

	if r.Policy.DispatchMode == routing.DispatchFanOut && len(r.Bindings) > txLimit {
		*errs = append(*errs, ValidationError{
			RouteID: r.ID,
			Rule:    "shared_outbox",
			Message: fmt.Sprintf("shared_outbox invalid: fan-out cardinality exceeds OutboxStore transaction limit (%d)", txLimit),
		})
	}
}

func validateMQTTQoS(r *RouteConfig, errs *ValidationErrors) {
	if !strings.EqualFold(r.TargetTransport, "mqtt") {
		return
	}

	isReliable := r.Policy.DeliveryMode == routing.DeliverySharedOutbox ||
		r.Policy.RequireDurableEgress

	if isReliable && r.TargetQoS == 0 {
		*errs = append(*errs, ValidationError{
			RouteID: r.ID,
			Rule:    "mqtt_qos",
			Message: "reliable MQTT route invalid: qos=0",
		})
	}
}
