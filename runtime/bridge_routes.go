package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// RegisterSessionSender registers a session and its sender for use as
// an egress target. This is needed when SharedOutbox routes fan out to
// sessions that are not the route's primary session (e.g. one SQS source
// writes to outbox partitions for multiple exclusive MQTT clients).
// The runtime creates a session.Manager and OutboxDrainer for each
// registered session during Start.
func (rt *Runtime) RegisterSessionSender(
	cfg session.Config,
	session ports.Session,
	sender ports.Sender,
) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.running {
		return errors.New("cannot register session sender on running runtime")
	}
	if cfg.SessionID == "" {
		return errors.New("session ID is required")
	}

	if _, exists := rt.sessionSenders[cfg.SessionID]; exists {
		return errors.New("duplicate session sender: " + cfg.SessionID)
	}

	rt.sessionSenders[cfg.SessionID] = &sessionSenderEntry{
		config:  cfg,
		session: session,
		sender:  sender,
	}
	return nil
}

// AddRoute registers a route with its transport instances and optional
// session configuration. The route is not started until Start is called.
func (rt *Runtime) AddRoute(
	cfg RouteConfig,
	receiver ports.Receiver,
	sender ports.Sender,
	session ports.Session,
	sessCfg *session.Config,
) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.running {
		return errors.New("cannot add route to running runtime")
	}

	for _, e := range rt.entries {
		if e.config.ID == cfg.ID {
			return errors.New("duplicate route ID: " + cfg.ID)
		}
	}

	rt.entries = append(rt.entries, &routeEntry{
		config:   cfg,
		receiver: receiver,
		sender:   sender,
		session:  session,
		sessCfg:  sessCfg,
	})
	return nil
}

// RouteInfo is the read-side route projection. It is defined in the
// ports package so driving adapters depend on the inner-ring contract,
// not on the runtime package. The alias is retained here so existing
// runtime callers keep compiling without an import-site rename.
type RouteInfo = ports.RouteInfo

// Routes returns information about all registered routes.
func (rt *Runtime) Routes() []ports.RouteInfo {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	infos := make([]ports.RouteInfo, len(rt.entries))
	for i, e := range rt.entries {
		infos[i] = ports.RouteInfo{
			ID:           e.config.ID,
			DeliveryMode: e.config.Policy.DeliveryMode,
			DispatchMode: e.config.Policy.DispatchMode,
			Policy:       e.config.Policy,
		}
	}
	return infos
}

// RouteLocator returns the cluster-aware route locator.
// Returns nil if no lease store is configured (standalone mode).
func (rt *Runtime) RouteLocator() ports.RouteLocator {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.locator == nil {
		return nil
	}
	return rt.locator
}

// Inject sends a synthetic message through the named route's delivery
// pipeline (processors, destination resolution, send/outbox). The
// envelope is cloned to prevent caller mutation. An ID is assigned if
// the envelope's ID field is empty.
func (rt *Runtime) Inject(ctx context.Context, routeID string, env *messaging.Envelope) error {
	return rt.injectToBinding(ctx, routeID, "", env, "")
}

// InjectToBinding is Inject with the dispatch confined to a single binding.
// The binding ID travels out-of-band on the synthetic delivery (never as a
// message header), so it survives the ingress reserved-header strip that
// removes any external x-bridge.route-override. Only trusted internal callers
// reach this path: the admin DLQ redrive uses it so replaying one failed
// fan-out leg re-persists a record (shared_outbox) or dispatches (direct_hold)
// for that binding ALONE, never the N-1 healthy destinations. An empty
// bindingID is equivalent to Inject. Returns shared.ErrNotFound when the route
// does not exist.
func (rt *Runtime) InjectToBinding(ctx context.Context, routeID, bindingID string, env *messaging.Envelope) error {
	return rt.injectToBinding(ctx, routeID, bindingID, env, "")
}

// InjectRedrive is the DLQ-redrive-safe variant of InjectToBinding: it
// re-issues the message under a FRESH envelope ID and carries the original ID
// out-of-band so the route runner stamps it as provenance
// (messaging.HeaderCausationID) after the ingress reserved-header strip.
//
// A redrive MUST NOT reuse the original envelope ID. The outbox retains
// completed/poisoned rows keyed UNIQUE(envelope_id, binding_id) as dedup
// evidence, so re-persisting under the original ID yields
// shared.ErrDuplicateRecord — which the shared_outbox dispatch path correctly
// treats as "already persisted" and ACKs. The redrive then reports success,
// the DLQ entry is deleted, and the message is never sent again: silent loss
// of both message and evidence. A fresh ID bypasses the dedup row (this is a
// NEW delivery attempt, deliberately re-issued by an operator) while the
// causation header preserves the audit link to the original envelope.
// Subject, payload and timestamps are preserved from the source envelope, as
// are the propagated bridge headers EXCEPT the transport dedup key: a redrive is
// a DELIBERATE operator re-issue, so x-bridge.dedup-id must NOT ride along (see
// below).
func (rt *Runtime) InjectRedrive(ctx context.Context, routeID, bindingID string, env *messaging.Envelope) error {
	originalID := env.ID()
	if originalID == "" {
		// Nothing to collide with in the outbox dedup index; a plain
		// binding-scoped inject (which assigns a fresh ID) is equivalent.
		return rt.injectToBinding(ctx, routeID, bindingID, env, "")
	}
	src := env.Clone()
	fresh, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:        generateID(),
		Subject:   src.Subject(),
		Payload:   src.Payload(),
		CreatedAt: src.CreatedAt(),
		ExpiresAt: src.ExpiresAt(),
	}, rt.clk.Now())
	if err != nil {
		return fmt.Errorf("runtime: inject redrive: fresh envelope: %w", err)
	}
	// StampHeaders is the trusted whole-map setter (no reserved-prefix strip):
	// propagated reserved headers (correlation-id, ordering-key, …) on the DLQ'd
	// envelope survive onto the re-issued one. The ingress strip in
	// doHandleDelivery still applies its normal posture afterwards.
	fresh.StampHeaders(src.HeadersSnapshot())
	// The stale transport dedup key MUST NOT ride along. x-bridge.dedup-id maps
	// to an idempotent/FIFO sender's dedup id (e.g. SQS FIFO
	// MessageDeduplicationId), whose whole job is to SUPPRESS re-delivery. Copied
	// onto the "fresh" envelope it would make the transport swallow the redrive
	// (ACK without delivering) → Send returns nil → the admin redrive would report
	// success and DELETE the DLQ entry after a no-op: silent evidence loss via
	// TRANSPORT dedup instead of outbox dedup — a fresh envelope ID alone does not
	// prevent it because the sender prefers a present dedup header over the ID.
	// Drop it so the sender re-derives dedup from the FRESH envelope ID. Ordering
	// and correlation keys are kept (they do not suppress delivery); provenance is
	// preserved via the causation link injectToBinding stamps from originalID.
	fresh.DeleteHeader(messaging.HeaderDeduplicationID)
	return rt.injectToBinding(ctx, routeID, bindingID, fresh, originalID)
}

func (rt *Runtime) injectToBinding(ctx context.Context, routeID, bindingID string, env *messaging.Envelope, redrivenFrom string) error {
	rt.mu.Lock()
	if !rt.running {
		rt.mu.Unlock()
		return fmt.Errorf("runtime is not running")
	}
	var entry *routeEntry
	for _, e := range rt.entries {
		if e.config.ID == routeID {
			entry = e
			break
		}
	}
	rt.mu.Unlock()

	if entry == nil {
		return shared.ErrNotFound
	}

	env = env.Clone()
	if env.ID() == "" {
		if err := env.AssignID(generateID()); err != nil {
			return err
		}
	}

	return entry.runner.HandleDelivery(ctx, &syntheticDelivery{env: env, binding: bindingID, redrivenFrom: redrivenFrom})
}

// syntheticDelivery implements ports.Delivery for programmatically
// injected messages that have no underlying transport.
type syntheticDelivery struct {
	env *messaging.Envelope
	// binding, when non-empty, confines dispatch to the named binding. It is
	// read by the route runner via the bindingOverrider interface AFTER the
	// ingress reserved-header strip, keeping this a trusted internal-only steer.
	binding string
	// redrivenFrom, when non-empty, is the ORIGINAL envelope ID of a DLQ
	// redrive re-issued under a fresh ID (Runtime.InjectRedrive). It travels
	// out-of-band for the same reason as binding: the runner stamps it as
	// provenance (HeaderCausationID) AFTER the ingress reserved-header strip,
	// so external messages can never spoof it.
	redrivenFrom string
}

func (d *syntheticDelivery) Envelope() *messaging.Envelope { return d.env }
func (d *syntheticDelivery) Ack(_ context.Context) error   { return nil }
func (d *syntheticDelivery) Retry(_ context.Context, _ time.Duration, _ error) error {
	return shared.ErrNotSupported
}
func (d *syntheticDelivery) Extend(_ context.Context, _ time.Time) error { return nil }

// BindingOverride satisfies the route runner's bindingOverrider contract,
// exposing the binding-scoped route override out-of-band (not via a header).
func (d *syntheticDelivery) BindingOverride() string { return d.binding }

// RedrivenFrom satisfies the route runner's redriveProvenancer contract,
// exposing the original envelope ID of a redriven message out-of-band.
func (d *syntheticDelivery) RedrivenFrom() string { return d.redrivenFrom }
