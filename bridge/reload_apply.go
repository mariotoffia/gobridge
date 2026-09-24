package bridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// InPlaceOutcome is what an in-place reload left the runtime running, and so
// what its caller does next.
type InPlaceOutcome int

const (
	// InPlaceApplied: the runtime runs the next configuration. The error is nil.
	InPlaceApplied InPlaceOutcome = iota
	// InPlaceUnchanged: the reload left the runtime's units as it found them:
	// no unit was retired or grafted, or every unit grafted was retired again
	// and every retired unit restored. The error says why the reload failed;
	// the caller keeps the runtime.
	//
	// It makes no claim that the runtime is running. A runtime stops or goes
	// terminal while a reload runs only through shutdown, a terminal component
	// failure or a configuration fence, and whoever did that owns it exactly as
	// without a reload: the shutdown path, the terminal backstop that restarts
	// the process (ADR-0004), or the path that withdrew the configuration.
	// Reporting torn instead would have the caller rebuild a terminal runtime
	// in-process, which no other terminal runtime gets.
	InPlaceUnchanged
	// InPlaceTorn: the runtime runs neither configuration, because a failure
	// after units were retired could not restore them, or the runtime stopped
	// running before the rest were retired. No ownership is in doubt. The caller
	// replaces the runtime: it stops it and builds the running configuration
	// afresh.
	InPlaceTorn
	// InPlaceWedged: a retired unit did not stop cleanly, or a part a serialized
	// reload built or restored did not stop, so whether its sessions still hold
	// their broker identities is unknown. The caller stops the runtime and wedges
	// instead of building anything that could claim them a second time
	// (ADR-0004).
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
// added unit.
//
// phase bounds one build phase: preparing the parts (with the preflight),
// committing them, or rebuilding the retired units after a failure. Apply calls
// it with ctx once per phase and cancels what it returns when the phase ends.
// Each phase gets a budget of its own because a serialized reload retires units
// between preparing and committing, and a retire may take the whole drain
// timeout; a budget spanning both would hand the commit a spent context. A nil
// phase runs every phase under ctx. Each retire, and each stop of a part never
// grafted, runs under r.DrainTimeout (the running configuration's drain timeout
// when not positive) detached from ctx, so a cancelled reload still leaves
// every unit settled.
//
// The caller serializes reloads of rt, but nothing serializes a Stop of rt with
// one: the watcher Start leaves on its context stops rt at shutdown whatever
// the caller holds. Stop leaves a unit being retired to its Retire and may
// close rt's stores before that Retire releases the unit's leases through them,
// so a reload caught by shutdown fails, and at worst reports wedged. A reload
// that finds rt not running before it retires anything reports unchanged, as it
// touched nothing, and leaves rt to whatever stopped it (see InPlaceUnchanged).
//
// The whole next document is validated and every added unit prepared before
// anything is retired, so a configuration a full build would refuse changes
// nothing. Then, when r is serialized, the retired units stop first and the
// parts are built after, so no exclusive broker identity is ever held twice;
// otherwise the parts are built while the retired units still serve, and a
// failed build changes nothing. A failure once a unit has retired restores the
// retired units from the running configuration, unless rt stopped running or
// something that may still hold an identity they claim did not stop (see
// restore).
func (r *InPlaceReload) Apply(ctx context.Context, rt *runtime.Runtime, newBuilder func(*ports.BridgeConfig) *Builder,
	phase func(context.Context) (context.Context, context.CancelFunc),
) (InPlaceOutcome, error) {
	if rt == nil || !rt.IsRunning() {
		return InPlaceUnchanged, errors.New("in-place reload: the runtime is not running")
	}
	if phase == nil {
		phase = func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} }
	}
	plans, err := r.prepareParts(ctx, rt, newBuilder, phase)
	if err != nil {
		return InPlaceUnchanged, err
	}

	var parts []*runtime.Runtime
	if !r.serialized {
		if parts, err = r.buildParts(ctx, plans, phase); err != nil {
			return InPlaceUnchanged, errors.Join(err, r.stopParts(ctx, parts))
		}
	}
	for i, u := range r.retire {
		if err := r.retireUnit(ctx, rt, u); err != nil {
			// A retire refused because rt stopped running took nothing out, so no
			// ownership is in doubt. Refused first, it changed nothing; refused
			// later, the units retired before it are gone.
			outcome := InPlaceWedged
			if errors.Is(err, runtime.ErrNotRunning) {
				outcome = InPlaceTorn
				if i == 0 {
					outcome = InPlaceUnchanged
				}
			}
			return outcome, errors.Join(err, r.stopParts(ctx, parts))
		}
	}
	if r.serialized {
		if parts, err = r.buildParts(ctx, plans, phase); err != nil {
			return r.restore(ctx, rt, newBuilder, phase, nil, parts, err)
		}
	}
	for i, part := range parts {
		if err := rt.Graft(part); err != nil {
			err = fmt.Errorf("in-place reload: graft unit (%v): %w", r.add[i], err)
			return r.restore(ctx, rt, newBuilder, phase, r.add[:i], parts[i:], err)
		}
	}
	return InPlaceApplied, nil
}

// prepareParts preflights the whole next document and plans the part of every
// added unit, in order, in one phase. It opens nothing.
func (r *InPlaceReload) prepareParts(ctx context.Context, rt *runtime.Runtime, newBuilder func(*ports.BridgeConfig) *Builder,
	phase func(context.Context) (context.Context, context.CancelFunc),
) ([]*BuildPlan, error) {
	ctx, cancel := phase(ctx)
	defer cancel()
	if err := newBuilder(r.next).Preflight(ctx); err != nil {
		return nil, fmt.Errorf("in-place reload: preflight: %w", err)
	}
	plans := make([]*BuildPlan, len(r.add))
	for i, u := range r.add {
		plan, err := newBuilder(u.sub).planPart(ctx, rt)
		if err != nil {
			return nil, fmt.Errorf("in-place reload: prepare unit (%v): %w", u, err)
		}
		plans[i] = plan
	}
	return plans, nil
}

// buildParts commits the plan of every added unit, in order, in one phase. On
// a failure it returns the parts already built with the error, for the caller
// to stop: whether a part that does not stop wedges the reload depends on
// whether it is serialized.
func (r *InPlaceReload) buildParts(ctx context.Context, plans []*BuildPlan,
	phase func(context.Context) (context.Context, context.CancelFunc),
) ([]*runtime.Runtime, error) {
	buildCtx, cancel := phase(ctx)
	defer cancel()
	parts := make([]*runtime.Runtime, 0, len(plans))
	for i, plan := range plans {
		part, err := plan.Commit(buildCtx)
		if err != nil {
			return parts, fmt.Errorf("in-place reload: build unit (%v): %w", r.add[i], err)
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// restore brings back the running configuration after a failure past retiring
// its units: it stops the parts that will not be grafted, retires the added
// units grafted so far, then builds each retired unit again from the running
// configuration, in one phase, and grafts it. cause is the failure that led
// here, and every outcome carries it.
//
// A grafted unit that does not retire cleanly wedges, as a retired unit does
// in Apply: its sessions may still hold the identities the restored units
// claim. When r is serialized, so does a part that does not stop: a serialized
// reload's parts may contend with the restored units for an exclusive
// identity, which some transports claim as a part is built. A build-first
// reload's parts contend for none, so restore carries on. For the same reason a
// serialized reload wedges, rather than tear, when a restored part whose graft
// is refused does not stop: the caller's torn handling would rebuild the
// running configuration and claim that part's identity beside it.
func (r *InPlaceReload) restore(ctx context.Context, rt *runtime.Runtime, newBuilder func(*ports.BridgeConfig) *Builder,
	phase func(context.Context) (context.Context, context.CancelFunc),
	grafted []reloadUnit, unused []*runtime.Runtime, cause error,
) (InPlaceOutcome, error) {
	if err := r.stopParts(ctx, unused); err != nil {
		cause = errors.Join(cause, err)
		if r.serialized {
			return InPlaceWedged, cause
		}
	}
	for _, u := range grafted {
		if err := r.retireUnit(ctx, rt, u); err != nil {
			return InPlaceWedged, errors.Join(cause, err)
		}
	}
	buildCtx, cancel := phase(ctx)
	defer cancel()
	for _, u := range r.retire {
		outcome := InPlaceTorn
		part, err := newBuilder(u.sub).buildPart(buildCtx, rt)
		if err == nil {
			if err = rt.Graft(part); err != nil {
				stopErr := r.stopParts(ctx, []*runtime.Runtime{part})
				if stopErr != nil && r.serialized {
					outcome = InPlaceWedged
				}
				err = errors.Join(err, stopErr)
			}
		}
		if err != nil {
			return outcome, errors.Join(cause, fmt.Errorf("in-place reload: restore unit (%v): %w", u, err))
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
	return context.WithTimeout(context.WithoutCancel(ctx), r.teardownBudget())
}

// teardownBudget is the drain timeout teardownCtx bounds a teardown by:
// r.DrainTimeout, or the running configuration's when that is not positive.
func (r *InPlaceReload) teardownBudget() time.Duration {
	if r.DrainTimeout > 0 {
		return r.DrainTimeout
	}
	return r.running.Bridge.DrainTimeoutDuration()
}

// String names u by its route and session ids, for errors.
func (u reloadUnit) String() string {
	return fmt.Sprintf("routes %q, sessions %q", u.routes, u.sessions)
}
