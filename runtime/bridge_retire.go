package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// Unit names the routes and sessions a reload retires together.
type Unit struct {
	Routes   []string
	Sessions []string
}

// ErrNotRunning is wrapped by a Retire or Graft that the runtime refuses
// because it is not running: never started, stopped, terminal or fenced. Such a
// refusal changes nothing, since nothing was taken out or moved in.
var ErrNotRunning = errors.New("runtime is not running")

// retiredUnit is what Retire took out of the runtime: the unit's registrations,
// the runs of its route runners, session managers and drainers, its managers by
// session id, its drainers, and the sessions no manager runs that only the unit
// held. It stays in rt.retiring until Retire has finished with it, so Fence
// still reaches its drainers, dlqToken still sees its managers' leases, and
// Graft still refuses its ids.
type retiredUnit struct {
	set       componentSet
	runs      []componentRun
	managers  map[string]*session.Manager
	drainers  []*drainerRun
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
// The unit leaves rt at once, so nothing new reaches it. Its
// in-flight deliveries then settle within the budget Stop uses, its route
// runners, drainers and session managers stop, its managers close (releasing
// their leases), and so does every session only the unit held: one no manager
// runs and no route left running was added with. Credential
// refreshers stop watching its transports no route left running holds, and one
// left watching nothing is closed. Until Retire has finished with the unit, a Fence still fences its
// drainers, and a DLQ write for one of its exclusive sessions is still fenced
// on that session's lease. Graft refuses the unit's ids until Retire returns:
// until then the retired ids' health records and exclusive marks are still
// being cleared.
//
// The unit is gone from rt even when Retire returns an error; the error names
// components that did not stop within ctx and sessions that failed to close.
// A component that did not stop keeps its drainers reachable by Fence, its
// exclusive sessions refusing DLQ writes, since nothing holds their lease, and
// the unit's ids refused by Graft, since it still runs under them. A
// close error is the caller's to act on: once every component has stopped,
// nothing writes under the unit's sessions, though an in-place reload wedges on
// any Retire error all the same.
func (rt *Runtime) Retire(ctx context.Context, u Unit) error {
	d, err := rt.detach(u)
	if err != nil {
		return err
	}
	var errs []error
	// Settle before cancelling, for the same reason Stop does: a cancelled send
	// fails its source ack and the broker redelivers a message already sent. A
	// delivery accepted between the in-flight check and the cancel is left
	// unsettled for the source to redeliver: the same at-least-once boundary
	// Stop accepts (broker redelivery, never a silent ack).
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
	// Refreshers let go of the transports before those are closed, so no
	// rotation that starts from here on reaches them. Forget does not wait for
	// a rotation already being applied: an MQTT or AMQP session refuses it once
	// closed, and any other transport at most swaps a client on a closed object.
	rt.forgetCredentialTargets(ctx, rt.releasedCredentialTargets(d))
	finished := waitRuns(ctx, d.runs)
	if !finished {
		errs = append(errs, fmt.Errorf("runtime: retire: routes, drainers and session managers "+
			"did not finish within the budget: %w", ctx.Err()))
		// A drainer's final drain may still be mid-send: give it the grace Stop
		// gives, so its Complete runs against a live lease.
		graceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rt.clampedStoreCloseGrace(ctx, d.set.entries))
		finished = waitRuns(graceCtx, d.runs)
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
	rt.finishRetire(d, finished)
	return errors.Join(errs...)
}

// detach takes the routes and sessions u names out of rt: its route entries,
// session senders, ingress sessions, session managers and their runs, drainers
// and locator registrations. Nothing new reaches them afterwards, and a Stop
// that follows leaves them to Retire. The unit joins rt.retiring.
func (rt *Runtime) detach(u Unit) (*retiredUnit, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.running || rt.stopped || rt.terminal || rt.fenced {
		return nil, fmt.Errorf("runtime: retire: %w", ErrNotRunning)
	}
	d := &retiredUnit{
		set: componentSet{
			sessionSenders:  make(map[string]*sessionSenderEntry),
			ingressSessions: make(map[string]*ingressSessionEntry),
		},
		managers: make(map[string]*session.Manager),
	}
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
	// No manager ties hand-wired routes added with one session object together,
	// so one of them may stay: it still rides on that session, which is left
	// open for whatever retires or stops the route that uses it last.
	d.unmanaged = slices.DeleteFunc(rt.unmanagedSessionRefsLocked(d.set), func(ref sessionRef) bool {
		return slices.ContainsFunc(kept, func(entry *routeEntry) bool {
			return ridesOnSessionObject(entry, []ports.Session{ref.sess})
		})
	})

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
			d.managers[sid] = mgr
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
			d.drainers = append(d.drainers, dr)
			d.runs = append(d.runs, dr.run)
			continue
		}
		drainers = append(drainers, dr)
	}
	rt.drainers = drainers
	rt.retiring = append(rt.retiring, d)
	return d, nil
}

// finishRetire forgets the unit once its sessions are closed. finished reports
// whether its components stopped. When they did, the unit leaves rt.retiring
// and its exclusive marks are cleared. When one did not, both stay: the
// straggler is still fenced by Fence, and a DLQ write for its session is still
// refused, now that its manager has closed and released the lease. The route
// and session health records are cleared either way, since a supervisor still
// winding down may otherwise leave a fault for a successor under the same id;
// a terminal runtime keeps the faults that ended it.
//
// Only what d took out is cleared, never every id the caller named: a second
// Retire naming a unit still draining takes nothing out, and clearing the
// draining unit's exclusive mark would let a DLQ write for its session through
// unfenced, while clearing its supervisors' records would hide a fault of a
// component still winding down.
func (rt *Runtime) finishRetire(d *retiredUnit, finished bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if finished {
		rt.retiring = slices.DeleteFunc(rt.retiring, func(r *retiredUnit) bool { return r == d })
		detached := make(map[string]bool)
		addSessionIDs(detached, d.set, d.managers)
		for sid := range detached {
			if _, successor := rt.sessionMgrs[sid]; !successor {
				delete(rt.exclusiveSessions, sid)
			}
		}
	}
	if rt.terminal {
		return
	}
	// Session health records are written by the supervisors of managers only.
	for sid := range d.managers {
		delete(rt.componentErrors, "session:"+sid)
	}
	for _, entry := range d.set.entries {
		name := "route:" + entry.config.ID
		delete(rt.componentErrors, name)
		delete(rt.routeFlaps, name)
		delete(rt.routeRunStart, name)
	}
}

// retiringManagerLocked returns the manager of session sid a Retire has taken
// out and not yet finished with. The caller holds rt.mu.
func (rt *Runtime) retiringManagerLocked(sid string) (*session.Manager, bool) {
	for _, u := range rt.retiring {
		if mgr, ok := u.managers[sid]; ok {
			return mgr, true
		}
	}
	return nil, false
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

// releasedCredentialTargets returns the credential targets of d that rt no
// longer holds. A hand-wired route left running may still ride on a session
// object d held (see detach), and that route's credentials must keep rotating.
func (rt *Runtime) releasedCredentialTargets(d *retiredUnit) []any {
	held := rt.CredentialTargets()
	return slices.DeleteFunc(d.set.credentialTargets(), func(target any) bool {
		return holdsTarget(held, target)
	})
}

// holdsTarget reports whether target is one of held, by identity. A target
// whose dynamic type is not comparable panics on ==; it counts as held, as a
// session object does in ridesOnSessionObject, so Retire keeps watching it
// instead of crashing.
func holdsTarget(held []any, target any) (holds bool) {
	defer func() {
		if recover() != nil {
			holds = true
		}
	}()
	return slices.Contains(held, target)
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
