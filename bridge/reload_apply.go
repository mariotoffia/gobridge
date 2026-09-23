package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// InPlaceOutcome is what an in-place reload left the runtime running, and so
// what its caller does next.
type InPlaceOutcome int

const (
	// InPlaceApplied: the runtime runs the next configuration. The error is nil.
	InPlaceApplied InPlaceOutcome = iota
	// InPlaceUnchanged: the runtime still runs the running configuration,
	// because nothing was retired or the retired units were restored. The error
	// says why the reload failed; the caller keeps the runtime.
	InPlaceUnchanged
	// InPlaceTorn: the runtime runs neither configuration, because a failure
	// after units were retired could not restore them. The caller replaces the
	// runtime: it stops it and builds the running configuration afresh.
	InPlaceTorn
	// InPlaceWedged: a retired unit did not stop cleanly, so whether its
	// sessions still hold their broker identities is unknown. The caller stops
	// the runtime and wedges instead of building anything that could claim them
	// a second time (ADR-0004).
	InPlaceWedged
)

// String names the outcome for logs and errors.
func (o InPlaceOutcome) String() string {
	switch o {
	case InPlaceApplied:
		return "applied"
	case InPlaceUnchanged:
		return "unchanged"
	case InPlaceTorn:
		return "torn"
	case InPlaceWedged:
		return "wedged"
	}
	return fmt.Sprintf("InPlaceOutcome(%d)", int(o))
}

// Apply reloads rt, a runtime running r's running configuration, to r's next
// configuration in place: the retired units leave rt and the added ones join
// it, while every other unit keeps running untouched.
//
// newBuilder returns a Builder for a configuration, set up the way the caller
// builds a full runtime (factories, processors, credential stores, validator).
// Apply uses it to preflight the whole next document and to build one part per
// added unit. ctx bounds the builds and is the caller's to bound. Each retire,
// and each stop of a part never grafted, runs under the drain timeout detached
// from ctx, so a cancelled reload still leaves every unit settled. The caller
// serializes reloads and stops of rt.
//
// The whole next document is validated and every added unit prepared before
// anything is retired, so a configuration a full build would refuse changes
// nothing. Then, when r is serialized, the retired units stop first and the
// parts are built after, so no exclusive broker identity is ever held twice;
// otherwise the parts are built while the retired units still serve, and a
// failed build changes nothing. A failure once a unit has retired restores the
// retired units from the running configuration.
func (r *InPlaceReload) Apply(ctx context.Context, rt *runtime.Runtime, newBuilder func(*ports.BridgeConfig) *Builder) (InPlaceOutcome, error) {
	if rt == nil || !rt.IsRunning() {
		return InPlaceUnchanged, errors.New("in-place reload: the runtime is not running")
	}
	if err := newBuilder(r.next).Preflight(ctx); err != nil {
		return InPlaceUnchanged, fmt.Errorf("in-place reload: preflight: %w", err)
	}
	plans := make([]*BuildPlan, len(r.add))
	for i, u := range r.add {
		plan, err := newBuilder(u.sub).planPart(ctx, rt)
		if err != nil {
			return InPlaceUnchanged, fmt.Errorf("in-place reload: prepare unit (%v): %w", u, err)
		}
		plans[i] = plan
	}

	var parts []*runtime.Runtime
	var err error
	if !r.serialized {
		if parts, err = r.buildParts(ctx, plans); err != nil {
			return InPlaceUnchanged, err
		}
	}
	for i, u := range r.retire {
		if err := r.retireUnit(ctx, rt, u); err != nil {
			// A first retire refused because rt stopped running took nothing out:
			// nothing changed and no ownership is in doubt.
			outcome := InPlaceWedged
			if i == 0 && errors.Is(err, runtime.ErrNotRunning) {
				outcome = InPlaceUnchanged
			}
			return outcome, errors.Join(err, r.stopParts(ctx, parts))
		}
	}
	if r.serialized {
		if parts, err = r.buildParts(ctx, plans); err != nil {
			return r.restore(ctx, rt, newBuilder, nil, nil, err)
		}
	}
	for i, part := range parts {
		if err := rt.Graft(part); err != nil {
			err = fmt.Errorf("in-place reload: graft unit (%v): %w", r.add[i], err)
			return r.restore(ctx, rt, newBuilder, r.add[:i], parts[i:], err)
		}
	}
	return InPlaceApplied, nil
}

// buildParts commits the plan of every added unit, in order. On a failure it
// stops the parts already built, so a failed build leaves nothing open.
func (r *InPlaceReload) buildParts(ctx context.Context, plans []*BuildPlan) ([]*runtime.Runtime, error) {
	parts := make([]*runtime.Runtime, 0, len(plans))
	for i, plan := range plans {
		part, err := plan.Commit(ctx)
		if err != nil {
			err = fmt.Errorf("in-place reload: build unit (%v): %w", r.add[i], err)
			return nil, errors.Join(err, r.stopParts(ctx, parts))
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// restore brings back the running configuration after a failure past retiring
// its units: it stops the parts that will not be grafted, retires the added
// units grafted so far, then builds each retired unit again from the running
// configuration and grafts it. cause is the failure that led here, and every
// outcome carries it.
//
// A grafted unit that does not retire cleanly wedges, as a retired unit does
// in Apply: its sessions may still hold the identities the restored units
// claim.
func (r *InPlaceReload) restore(ctx context.Context, rt *runtime.Runtime, newBuilder func(*ports.BridgeConfig) *Builder,
	grafted []reloadUnit, unused []*runtime.Runtime, cause error,
) (InPlaceOutcome, error) {
	cause = errors.Join(cause, r.stopParts(ctx, unused))
	for _, u := range grafted {
		if err := r.retireUnit(ctx, rt, u); err != nil {
			return InPlaceWedged, errors.Join(cause, err)
		}
	}
	for _, u := range r.retire {
		part, err := newBuilder(u.sub).buildPart(ctx, rt)
		if err == nil {
			if err = rt.Graft(part); err != nil {
				err = errors.Join(err, r.stopParts(ctx, []*runtime.Runtime{part}))
			}
		}
		if err != nil {
			return InPlaceTorn, errors.Join(cause, fmt.Errorf("in-place reload: restore unit (%v): %w", u, err))
		}
	}
	return InPlaceUnchanged, cause
}

// retireUnit retires u from rt.
func (r *InPlaceReload) retireUnit(ctx context.Context, rt *runtime.Runtime, u reloadUnit) error {
	retireCtx, cancel := r.teardownCtx(ctx)
	defer cancel()
	if err := rt.Retire(retireCtx, runtime.Unit{Routes: u.routes, Sessions: u.sessions}); err != nil {
		return fmt.Errorf("in-place reload: retire unit (%v): %w", u, err)
	}
	return nil
}

// stopParts stops parts that were built and will not be grafted, releasing
// their sessions, receivers and senders. The stores they borrowed stay open
// for the runtime that owns them.
func (r *InPlaceReload) stopParts(ctx context.Context, parts []*runtime.Runtime) error {
	var errs []error
	for _, part := range parts {
		stopCtx, cancel := r.teardownCtx(ctx)
		if err := part.Stop(stopCtx); err != nil {
			errs = append(errs, fmt.Errorf("in-place reload: stop part never grafted: %w", err))
		}
		cancel()
	}
	return errors.Join(errs...)
}

// teardownCtx bounds a retire or a part stop by the drain timeout, detached
// from ctx: a teardown that starts must finish, or a retired unit's in-flight
// deliveries and a stopped part's sessions are abandoned mid-way.
func (r *InPlaceReload) teardownCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), r.running.Bridge.DrainTimeoutDuration())
}

// String names u by its route and session ids, for errors.
func (u reloadUnit) String() string {
	return fmt.Sprintf("routes %q, sessions %q", u.routes, u.sessions)
}
