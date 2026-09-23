package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/mariotoffia/gobridge/runtime/session"
)

// Unit names the routes and sessions a reload retires together.
type Unit struct {
	Routes   []string
	Sessions []string
}

// retiredUnit is what Retire took out of the runtime: the unit's registrations,
// the runs of its route runners, session managers and drainers, its managers,
// and the sessions no manager runs that only the unit held.
type retiredUnit struct {
	set       componentSet
	runs      []componentRun
	managers  []*session.Manager
	unmanaged []sessionRef
}

// Retire drains, stops and removes the routes and sessions u names, while every
// other component keeps running untouched. It is Graft's inverse: a reload
// retires the units whose configuration changed, then grafts their successors.
//
// u must be closed over its sessions, as a grafted part is: it names every
// session its routes ride on, bind to or hold as their primary session, and no
// route left running uses a session it names. Ids rt does not have are ignored.
//
// The unit leaves rt at once, so its ids are free for a successor. Its
// in-flight deliveries then settle within the budget Stop uses, its route
// runners, drainers and session managers stop, its managers close (releasing
// their leases), and so does every session only the unit held. Credential
// refreshers stop watching its transports, and one left watching nothing is
// closed. Graft the successor after Retire returns: until then the retired ids'
// health records and exclusive marks are still being cleared.
//
// The unit is gone from rt even when Retire returns an error; the error names
// components that did not stop within ctx and sessions that failed to close.
func (rt *Runtime) Retire(ctx context.Context, u Unit) error {
	d, err := rt.detach(u)
	if err != nil {
		return err
	}
	var errs []error
	// Settle before cancelling, for the same reason Stop does: a cancelled send
	// fails its source ack and the broker redelivers a message already sent.
	if budget := rt.stopDrainBudget(); budget > 0 && ctx.Err() == nil && anyInFlight(d.set.entries) {
		qCtx, cancel := context.WithTimeout(ctx, budget)
		snapshot := func() []*routeEntry { return d.set.entries }
		if err := rt.waitEntriesQuiescent(qCtx, snapshot, QuiescenceOptions{}); err != nil && rt.logger != nil {
			rt.logger.Warn("retire drain did not settle in-flight deliveries before deadline; cancelling (unsettled sources rely on broker redelivery)",
				"routes", u.Routes, "budget", budget, "error", err)
		}
		cancel()
	}
	for _, run := range d.runs {
		if run.cancel != nil {
			run.cancel()
		}
	}
	// Refreshers let go of the transports before those are closed, so a
	// rotation is never applied to a session mid-close (as in Stop).
	rt.forgetCredentialTargets(ctx, d.set.credentialTargets())
	if !waitRuns(ctx, d.runs) {
		errs = append(errs, fmt.Errorf("runtime: retire: routes, drainers and session managers "+
			"did not finish within the budget: %w", ctx.Err()))
		// A drainer's final drain may still be mid-send: give it the grace Stop
		// gives, so its Complete runs against a live lease.
		graceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rt.clampedStoreCloseGrace(ctx, d.set.entries))
		waitRuns(graceCtx, d.runs)
		cancel()
	}

	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rt.closeTimeout())
	defer cancel()
	for _, mgr := range d.managers {
		if err := mgr.Close(closeCtx); err != nil {
			errs = append(errs, fmt.Errorf("runtime: retire: closing session manager: %w", err))
		}
	}
	for _, ref := range d.unmanaged {
		if err := ref.sess.Close(closeCtx); err != nil {
			errs = append(errs, fmt.Errorf("runtime: retire: closing unmanaged session %q: %w", ref.sid, err))
		}
	}
	rt.clearRetired(u, d.set.entries)
	return errors.Join(errs...)
}

// detach takes the routes and sessions u names out of rt: its route entries,
// session senders, ingress sessions, session managers and their runs, drainers
// and locator registrations. Nothing new reaches them afterwards, and a Stop
// that follows leaves them to Retire.
func (rt *Runtime) detach(u Unit) (retiredUnit, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.running || rt.stopped || rt.terminal || rt.fenced {
		return retiredUnit{}, errors.New("runtime: retire: runtime is not running")
	}
	d := retiredUnit{set: componentSet{
		sessionSenders:  make(map[string]*sessionSenderEntry),
		ingressSessions: make(map[string]*ingressSessionEntry),
	}}
	// A new slice, never filtered in place: readers that took rt.entries under
	// the lock iterate it after releasing the lock.
	var kept []*routeEntry
	for _, entry := range rt.entries {
		if !slices.Contains(u.Routes, entry.config.ID) {
			kept = append(kept, entry)
			continue
		}
		d.set.entries = append(d.set.entries, entry)
		d.runs = append(d.runs, entry.run)
	}
	for _, sid := range u.Sessions {
		if sse, ok := rt.sessionSenders[sid]; ok {
			d.set.sessionSenders[sid] = sse
		}
		if ise, ok := rt.ingressSessions[sid]; ok {
			d.set.ingressSessions[sid] = ise
		}
	}
	// Resolved while rt still holds the unit, so a session any manager runs —
	// the unit's or a survivor's — is never taken for one only the unit held.
	d.unmanaged = rt.unmanagedSessionRefsLocked(d.set)

	rt.entries = kept
	if rt.locator != nil {
		for _, entry := range d.set.entries {
			rt.locator.UnregisterRoute(entry.config.ID)
		}
	}
	for _, sid := range u.Sessions {
		delete(rt.sessionSenders, sid)
		delete(rt.ingressSessions, sid)
		if mgr, ok := rt.sessionMgrs[sid]; ok {
			d.managers = append(d.managers, mgr)
			delete(rt.sessionMgrs, sid)
		}
		if run, ok := rt.sessionRuns[sid]; ok {
			d.runs = append(d.runs, run)
			delete(rt.sessionRuns, sid)
		}
	}
	var drainers []*drainerRun
	for _, dr := range rt.drainers {
		if slices.Contains(u.Sessions, dr.sessionID) {
			d.runs = append(d.runs, dr.run)
			continue
		}
		drainers = append(drainers, dr)
	}
	rt.drainers = drainers
	return d, nil
}

// clearRetired forgets the unit's exclusive marks and route and session health
// records once its components have stopped and its sessions are closed. Until
// then an exclusive session stays marked, so a DLQ write for it is refused
// rather than written unfenced while it has no manager here; and a supervisor
// still winding down may record a fault again. A terminal runtime keeps the
// faults that ended it.
func (rt *Runtime) clearRetired(u Unit, entries []*routeEntry) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, sid := range u.Sessions {
		if _, successor := rt.sessionMgrs[sid]; !successor {
			delete(rt.exclusiveSessions, sid)
		}
	}
	if rt.terminal {
		return
	}
	for _, sid := range u.Sessions {
		delete(rt.componentErrors, "session:"+sid)
	}
	for _, entry := range entries {
		name := "route:" + entry.config.ID
		delete(rt.componentErrors, name)
		delete(rt.routeFlaps, name)
		delete(rt.routeRunStart, name)
	}
}

// waitRuns waits until every run is done, and reports false when ctx ends
// first.
func waitRuns(ctx context.Context, runs []componentRun) bool {
	for _, run := range runs {
		if run.done == nil || isClosed(run.done) {
			continue
		}
		select {
		case <-run.done:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// credentialTargets lists every session, receiver and sender of set: the
// transports a credential refresher may watch.
func (s componentSet) credentialTargets() []any {
	var targets []any
	add := func(target any) {
		if target != nil {
			targets = append(targets, target)
		}
	}
	for _, entry := range s.entries {
		add(entry.session)
		add(entry.receiver)
		add(entry.sender)
		for _, sender := range entry.config.Senders {
			add(sender)
		}
	}
	for _, sse := range s.sessionSenders {
		add(sse.session)
		add(sse.sender)
	}
	for _, ise := range s.ingressSessions {
		add(ise.session)
	}
	return targets
}

// forgetCredentialTargets asks every credential refresher to stop watching
// targets, then closes and drops each one left watching nothing. The refreshers
// are called outside rt.mu, since they take locks of their own; a hook Stop
// has already taken is left to Stop, so each is closed exactly once.
func (rt *Runtime) forgetCredentialTargets(ctx context.Context, targets []any) {
	if len(targets) == 0 {
		return
	}
	rt.mu.Lock()
	hooks := slices.Clone(rt.credHooks)
	rt.mu.Unlock()
	idle := make(map[*credentialHook]bool)
	for _, hook := range hooks {
		if hook.forget != nil && hook.forget(targets) {
			idle[hook] = true
		}
	}
	if len(idle) == 0 {
		return
	}
	var closing, kept []*credentialHook
	rt.mu.Lock()
	for _, hook := range rt.credHooks {
		if idle[hook] {
			closing = append(closing, hook)
		} else {
			kept = append(kept, hook)
		}
	}
	rt.credHooks = kept
	rt.mu.Unlock()
	closeCredentialHooks(ctx, closing, rt.closeTimeout())
}
