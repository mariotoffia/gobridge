package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// Stores is the set of stores a runtime was built with.
type Stores struct {
	Lease                ports.LeaseStore
	Outbox               ports.OutboxStore
	DLQ                  ports.DLQStore
	ManagedSubscriptions ports.ManagedSubscriptionStore
}

// Stores returns the stores this runtime was built with, so a part can be built
// over them and grafted onto it. The options passed to New set them, and
// nothing changes them afterwards, so no lock is needed.
func (rt *Runtime) Stores() Stores {
	return Stores{
		Lease:                rt.leaseStore,
		Outbox:               rt.outboxStore,
		DLQ:                  rt.dlqStore,
		ManagedSubscriptions: rt.managedSubscriptionStore,
	}
}

// WithSharedStores marks the stores as owned by another runtime: Stop leaves
// them open. Every part built for Graft carries it, because the part borrows
// the stores of the runtime it joins, and that runtime closes them.
func WithSharedStores() Option {
	return func(rt *Runtime) { rt.sharedStores = true }
}

// Graft moves the routes, sessions and credential hooks of part into rt and
// starts them, while every component rt already runs keeps running untouched.
// part must be built over rt.Stores() with WithSharedStores and never started,
// and may not reuse a route id or a session id rt already has, nor one of a
// unit still being retired — its Retire has not returned, or left a component
// running — since a straggler may still run under it. The grafted
// components run with rt's settings (clock, metrics, logger, lease owner,
// instance id), not with part's.
//
// part must be closed over its sessions: no route of part may ride on
// (SourceSessionID) or bind to (a binding's SessionID) a session rt already
// has, and no route of rt may ride on or bind to a session part brings. A
// wiring pass wires a session together with the routes that come with it, so
// such a route would get no drainer or settlement barrier for the other side's
// session, and the two sides would no longer be separate reload units. Graft
// refuses a part that breaks this where the route names the session by id.
//
// On success part is consumed: it holds nothing, a later Start fails, and a
// later Stop is a no-op. On a refusal part is left exactly as it was, and the
// caller Stops it.
//
// Lock order is rt.mu, then part.mu. part is private to the caller until it is
// grafted, so nothing takes the two locks in the other order.
func (rt *Runtime) Graft(part *Runtime) error {
	if part == nil || part == rt {
		return errors.New("runtime: graft: part is nil or the runtime itself")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.running || rt.stopped || rt.terminal || rt.fenced {
		return fmt.Errorf("runtime: graft: %w", ErrNotRunning)
	}
	part.mu.Lock()
	defer part.mu.Unlock()
	if part.running || part.stopped || part.consumed || part.terminal || part.fenced {
		return errors.New("runtime: graft: part must be built and never started")
	}
	if !part.sharedStores || !sameStores(rt.Stores(), part.Stores()) {
		return errors.New("runtime: graft: part must be built over this runtime's stores with WithSharedStores")
	}
	if err := rt.graftCollisionLocked(part); err != nil {
		return err
	}
	// The checks Start runs, over rt's routes and session senders together with
	// part's: a graft must never admit what a Start of both would refuse, such as
	// two routes that drain one outbox partition under different policies.
	entries := append(slices.Clone(rt.entries), part.entries...)
	if err := validateRoutes(entries, rt.outboxStore != nil, rt.leaseStore != nil, rt.dlqStore != nil); err != nil {
		return err
	}
	senders := maps.Clone(rt.sessionSenders)
	maps.Copy(senders, part.sessionSenders)
	if err := rt.sharedOutboxDrainerConflicts(entries, senders); err != nil {
		return err
	}

	// The wiring pass resolves each session, and the route it feeds, through
	// rt's collections, so part's are registered in rt before the pass runs.
	set := componentSet{entries: part.entries, sessionSenders: part.sessionSenders, ingressSessions: part.ingressSessions}
	rt.entries = append(rt.entries, part.entries...)
	maps.Copy(rt.sessionSenders, part.sessionSenders)
	maps.Copy(rt.ingressSessions, part.ingressSessions)
	rt.credHooks = append(rt.credHooks, part.credHooks...)
	rt.startComponentsLocked(set)

	part.entries = nil
	part.sessionSenders = make(map[string]*sessionSenderEntry)
	part.ingressSessions = make(map[string]*ingressSessionEntry)
	part.credHooks = nil
	part.consumed = true
	return nil
}

// sameStores reports whether a and b hold the same store instances. A store
// whose dynamic type is not comparable panics on ==; it counts as different, so
// the check refuses instead of crashing its caller.
func sameStores(a, b Stores) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// graftCollisionLocked refuses a part that reuses a route id or a session id of
// rt — a route id names one route runner, and a session has exactly one manager
// — or of a unit still being retired, whose stragglers may still run under it,
// or that is not closed over its sessions (see Graft). The caller holds rt.mu
// and part.mu.
func (rt *Runtime) graftCollisionLocked(part *Runtime) error {
	routes := make(map[string]bool, len(rt.entries))
	for _, entry := range rt.entries {
		routes[entry.config.ID] = true
	}
	retiringRoutes, retiringSessions := rt.retiringIDsLocked()
	for _, entry := range part.entries {
		if routes[entry.config.ID] {
			return fmt.Errorf("runtime: graft: route %q is already registered", entry.config.ID)
		}
		if retiringRoutes[entry.config.ID] {
			return fmt.Errorf("runtime: graft: route %q is still being retired", entry.config.ID)
		}
	}
	sessions := rt.sessionIDsLocked()
	partSessions := part.sessionIDsLocked()
	for sid := range partSessions {
		if sessions[sid] {
			return fmt.Errorf("runtime: graft: session %q is already registered", sid)
		}
		if retiringSessions[sid] {
			return fmt.Errorf("runtime: graft: session %q is still being retired", sid)
		}
	}
	for _, entry := range part.entries {
		if sid := usedSessionIn(entry, sessions); sid != "" {
			return fmt.Errorf("runtime: graft: part route %q uses session %q of the runtime; "+
				"a part must bring every session its routes use", entry.config.ID, sid)
		}
	}
	for _, entry := range rt.entries {
		if sid := usedSessionIn(entry, partSessions); sid != "" {
			return fmt.Errorf("runtime: graft: route %q uses session %q the part brings; "+
				"retire the route with the unit that brings its session", entry.config.ID, sid)
		}
	}
	return nil
}

// usedSessionIn returns the session of ids that entry's route rides on or binds
// to, or "" when it uses none of them.
func usedSessionIn(entry *routeEntry, ids map[string]bool) string {
	used := []string{entry.config.SourceSessionID}
	for _, binding := range entry.config.Bindings {
		used = append(used, binding.SessionID)
	}
	for _, sid := range used {
		if sid != "" && ids[sid] {
			return sid
		}
	}
	return ""
}

// sessionIDsLocked returns every session id rt knows: managed, registered as a
// session sender or an ingress session, or named by a route's session block.
// The caller holds rt.mu.
func (rt *Runtime) sessionIDsLocked() map[string]bool {
	ids := make(map[string]bool)
	addSessionIDs(ids, componentSet{entries: rt.entries, sessionSenders: rt.sessionSenders, ingressSessions: rt.ingressSessions}, rt.sessionMgrs)
	return ids
}

// retiringIDsLocked returns the route ids and the session ids of every unit a
// Retire has taken out and not finished with. The caller holds rt.mu.
func (rt *Runtime) retiringIDsLocked() (routes, sessions map[string]bool) {
	routes, sessions = make(map[string]bool), make(map[string]bool)
	for _, u := range rt.retiring {
		for _, entry := range u.set.entries {
			routes[entry.config.ID] = true
		}
		addSessionIDs(sessions, u.set, u.managers)
	}
	return routes, sessions
}

// addSessionIDs adds to ids every session id of set and managers: managed,
// registered as a session sender or an ingress session, or named by a route's
// session block.
func addSessionIDs(ids map[string]bool, set componentSet, managers map[string]*session.Manager) {
	for sid := range managers {
		ids[sid] = true
	}
	for sid := range set.sessionSenders {
		ids[sid] = true
	}
	for sid := range set.ingressSessions {
		ids[sid] = true
	}
	for _, entry := range set.entries {
		if entry.sessCfg != nil {
			ids[entry.sessCfg.SessionID] = true
		}
	}
}

// credentialHook is one credential refresher's hold on the runtime: Stop calls
// close, and forget drops targets the runtime no longer runs.
type credentialHook struct {
	close  func(context.Context)
	forget func(targets []any) (idle bool) // nil for a hook attached by AttachCredentialCloser only
}

// AttachCredentialCloser registers a close-on-stop hook with the runtime.
// Each call adds a hook. The runtime invokes every hook during Stop, before
// session teardown, so any goroutines that call ApplyCredentials on a session
// can be cancelled safely. A closer receives a bounded ctx; honouring it lets
// the runtime cap Stop latency when a watcher is unresponsive.
//
// Accepting a closure (rather than an interface value) deliberately keeps
// runtime free of any structural reference to a caller-defined type:
// the runtime sees only func(context.Context); deep architecture
// analysis cannot infer a phantom dependency on the caller's package.
func (rt *Runtime) AttachCredentialCloser(close func(context.Context)) {
	if rt == nil || close == nil {
		return
	}
	rt.mu.Lock()
	rt.credHooks = append(rt.credHooks, &credentialHook{close: close})
	rt.mu.Unlock()
}

// AttachCredentialForget registers the forget half of a credential refresher.
// forget stops the refresher from watching targets — sessions, receivers and
// senders the runtime no longer runs — and reports whether the refresher is
// left watching nothing, so a runtime that removes some of its components
// while it keeps running can stop rotating credentials into closed transports
// and close a refresher that has gone idle.
//
// The builder attaches a refresher's closer and then its forget, so forget
// joins the last hook while that hook has none, and otherwise starts a hook of
// its own.
func (rt *Runtime) AttachCredentialForget(forget func(targets []any) (idle bool)) {
	if rt == nil || forget == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if last := len(rt.credHooks) - 1; last >= 0 && rt.credHooks[last].forget == nil {
		rt.credHooks[last].forget = forget
		return
	}
	rt.credHooks = append(rt.credHooks, &credentialHook{forget: forget})
}

// CredentialTargets lists every session, receiver and sender rt holds. A
// Retire hands its unit's share of them to each forget, so a credential
// refresher that watches only these goes idle once its units have retired.
func (rt *Runtime) CredentialTargets() []any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return componentSet{entries: rt.entries, sessionSenders: rt.sessionSenders, ingressSessions: rt.ingressSessions}.credentialTargets()
}

// closeCredentialHooks runs the close of every hook concurrently and waits for
// them at most timeout, or until ctx ends, so a stuck closer can neither hold
// its caller past that budget nor keep another refresher open.
func closeCredentialHooks(ctx context.Context, hooks []*credentialHook, timeout time.Duration) {
	if len(hooks) == 0 {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	// The closers have an explicit lifetime: when they overrun the bounded
	// timer or the caller's ctx, the caller moves on (best-effort).
	done := make(chan struct{})
	go func() {
		defer close(done)
		var closers sync.WaitGroup
		for _, hook := range hooks {
			if hook.close != nil {
				closers.Go(func() { hook.close(closeCtx) })
			}
		}
		closers.Wait()
	}()
	select {
	case <-done:
	case <-closeCtx.Done():
	case <-ctx.Done():
	}
}
