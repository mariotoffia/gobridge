package runtime

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime/debug"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// autoRedriveState is the runtime's automatic-redrive configuration and the
// lock that runs one redrive pass at a time (ADR 0019).
type autoRedriveState struct {
	window time.Duration
	mu     sync.Mutex
}

const (
	// autoRedriveReadyPoll is how often a pending automatic redrive re-checks
	// that the runtime can deliver.
	autoRedriveReadyPoll = time.Second
	// autoRedrivePage is how many records one DLQ list call returns.
	autoRedrivePage = 100
	// autoRedriveStoreTimeout bounds each DLQ store call of a pass.
	autoRedriveStoreTimeout = 30 * time.Second
)

// autoRedriveTrigger names a system event that may redrive dead-letter records.
type autoRedriveTrigger string

// triggerSubscriptionAdded fires when a managed session's broker grants a
// subscription that was not in the session's history.
const triggerSubscriptionAdded autoRedriveTrigger = "subscription_added"

// autoRedriveRule pairs a trigger with the record type (ErrorCode) it may
// redrive and the ExtraInfo keys the event and the record must agree on.
type autoRedriveRule struct {
	trigger   autoRedriveTrigger
	errorCode shared.ErrorCode
	keys      []string
}

// autoRedriveEvent is one occurrence of a trigger on a session, with one fact
// set per thing it reports (for subscription_added, per added filter).
type autoRedriveEvent struct {
	trigger   autoRedriveTrigger
	sessionID string
	facts     []map[string]string
}

// autoRedriveRules is the registry of redrive rules: each pairs a trigger with
// the record type (ErrorCode) it may redrive and the ExtraInfo keys the event
// and the record must agree on. Adding a pair adds a rule; the mechanism does
// not change (ADR 0019).
func autoRedriveRules() []autoRedriveRule {
	return []autoRedriveRule{{
		trigger:   triggerSubscriptionAdded,
		errorCode: shared.ErrCodeSubscriptionRemoved,
		keys:      []string{routing.ExtraInfoSessionID, routing.ExtraInfoManagedIdentity, routing.ExtraInfoSubscription},
	}}
}

// matches reports whether e is a record ev may redrive: an automatic record of
// a rule's type whose facts agree with one of the event's fact sets on every
// rule key. An empty value never matches, so a record or event missing a fact
// is left alone.
func (ev autoRedriveEvent) matches(e routing.DLQEntry) bool {
	if e.RedriveMode() != routing.RedriveAuto {
		return false
	}
	info := e.ExtraInfo()
	for _, rule := range autoRedriveRules() {
		if rule.trigger != ev.trigger || e.ErrorCode() != string(rule.errorCode) {
			continue
		}
		for _, facts := range ev.facts {
			if factsAgree(rule.keys, facts, info) {
				return true
			}
		}
	}
	return false
}

// factsAgree reports whether a and b hold the same non-empty value for every key.
func factsAgree(keys []string, a, b map[string]string) bool {
	for _, k := range keys {
		if a[k] == "" || a[k] != b[k] {
			return false
		}
	}
	return true
}

// WithAutoRedriveWindow sets how old a dead-letter record may be and still be
// redriven by a matching system event by itself (ADR 0019). Zero or a negative
// value turns automatic redrive off. Without the option the window is
// ports.DefaultAutoRedriveWindow.
func WithAutoRedriveWindow(d time.Duration) Option {
	return func(rt *Runtime) { rt.autoRedrive.window = max(d, 0) }
}

// installAutoRedriveTrigger gives every session of sessionIDs that reports a
// managed identity the subscription-added hook (ADR 0019). Nothing is installed
// without a DLQ store or with the window off. The hook never takes rt.mu: the
// session calls it from inside a reconcile, so it only starts the pass, as a
// background component Stop waits for. Caller holds rt.mu.
func (rt *Runtime) installAutoRedriveTrigger(sessionIDs []string) {
	if rt.dlqStore == nil || rt.autoRedrive.window <= 0 {
		return
	}
	workCtx := rt.workCtx
	for _, sid := range sessionIDs {
		sess := rt.managedSession(sid)
		hooked, ok := sess.(ports.SubscriptionAddedHookConfigurer)
		reporter, reports := sess.(ports.ManagedSubscriptionIdentityReporter)
		if !ok || !reports {
			continue
		}
		identity := reporter.ManagedSubscriptionIdentity()
		if identity == "" {
			continue
		}
		sessionID := sid
		hooked.SetSubscriptionAddedHook(func(filters []string) {
			// A runtime that is stopping starts no new work.
			if len(filters) == 0 || workCtx.Err() != nil {
				return
			}
			ev := autoRedriveEvent{trigger: triggerSubscriptionAdded, sessionID: sessionID}
			for _, f := range filters {
				ev.facts = append(ev.facts, map[string]string{
					routing.ExtraInfoSessionID:       sessionID,
					routing.ExtraInfoManagedIdentity: identity,
					routing.ExtraInfoSubscription:    f,
				})
			}
			rt.startBackground(workCtx, "auto-redrive:"+sessionID, func(ctx context.Context) error {
				rt.runAutoRedrive(ctx, ev)
				// Never an error: a failed redrive keeps its records and must
				// not make the runtime terminal.
				return nil
			})
		})
	}
}

// runAutoRedrive is one automatic redrive (ADR 0019): it waits until the
// runtime can deliver through the session's ingress route, then, holding the
// pass lock so two events cannot redrive one record twice, redrives every
// record of that route inside the window that ev matches.
func (rt *Runtime) runAutoRedrive(ctx context.Context, ev autoRedriveEvent) {
	routeID, ok := rt.awaitAutoRedriveRoute(ctx, ev.sessionID)
	if !ok {
		return
	}
	rt.autoRedrive.mu.Lock()
	defer rt.autoRedrive.mu.Unlock()
	redriven, failed := rt.autoRedrivePass(ctx, routeID, ev)
	logging.DebugContext(rt.logger, ctx, "automatic redrive pass finished",
		"session_id", ev.sessionID, "route_id", routeID, "redriven", redriven, "failed", failed)
}

// awaitAutoRedriveRoute re-checks every autoRedriveReadyPoll until the runtime
// is at least subscribed and the ingress route of session sid has a started
// runner, and names that route. It reports false when ctx ends first, or when
// the runtime is ready but the session has no single ingress route: the records
// then stay for an operator.
func (rt *Runtime) awaitAutoRedriveRoute(ctx context.Context, sid string) (string, bool) {
	for {
		routeID, ready := rt.autoRedriveTarget(ctx, sid)
		if ready && routeID == "" {
			if rt.logger != nil {
				rt.logger.Warn("automatic redrive skipped: session has no single ingress route", "session_id", sid)
			}
			return "", false
		}
		if ready {
			return routeID, true
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-rt.clk.After(autoRedriveReadyPoll):
		}
	}
}

// autoRedriveTarget is one readiness check of awaitAutoRedriveRoute. ready with
// an empty routeID means the runtime is ready and sid has no single ingress route.
func (rt *Runtime) autoRedriveTarget(ctx context.Context, sid string) (routeID string, ready bool) {
	if rt.ReadinessLevel(ctx) < ports.LevelSubscribed {
		return "", false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	routeID = rt.sourceRouteOn(sid)
	for _, e := range rt.entries {
		if e.config.ID == routeID {
			return routeID, e.runner != nil && isClosed(e.runner.Started())
		}
	}
	return "", true
}

// autoRedrivePass lists the route's records inside the window that failed less
// than a millisecond after the pass started, oldest first, and redrives each one
// ev matches. It pages forward from the last FailedAt it saw, skipping records
// it has already seen, and stops on a short page, a page with nothing new, a
// store error, or a failure the route did not settle itself. The Before bound
// keeps a route that dead-letters during the pass from paging it forever.
// Caller holds rt.autoRedrive.mu.
func (rt *Runtime) autoRedrivePass(ctx context.Context, routeID string, ev autoRedriveEvent) (redriven, failed int) {
	start := rt.clk.Now()
	// Before is exclusive and stores keep FailedAt to the millisecond, so the
	// bound is one millisecond past the start: a record written in the pass's
	// own millisecond is still included.
	filter := routing.DLQFilter{
		RouteID: routeID,
		Since:   start.Add(-rt.autoRedrive.window),
		Before:  start.Add(time.Millisecond),
		Limit:   autoRedrivePage,
	}
	seen := make(map[string]struct{})
	for {
		listCtx, cancel := context.WithTimeout(ctx, autoRedriveStoreTimeout)
		page, err := rt.dlqStore.List(listCtx, filter)
		cancel()
		if err != nil {
			if rt.logger != nil {
				rt.logger.Warn("automatic redrive stopped: listing DLQ records failed",
					"session_id", ev.sessionID, "route_id", routeID, "error", err)
			}
			return redriven, failed
		}
		fresh := false
		for _, e := range page {
			if _, dup := seen[e.ID()]; dup {
				continue
			}
			seen[e.ID()] = struct{}{}
			fresh = true
			filter.Since = e.FailedAt()
			if !ev.matches(e) {
				continue
			}
			ok, stop := rt.autoRedriveOne(ctx, routeID, ev.sessionID, e)
			if ok {
				redriven++
			} else {
				failed++
			}
			if stop {
				return redriven, failed
			}
		}
		if !fresh || len(page) < autoRedrivePage {
			return redriven, failed
		}
	}
}

// autoRedriveOne redrives record e inject-then-delete (ADR 0015), counts and
// audits it, and reports whether it was redriven and whether the pass must
// stop. A failed inject keeps the record as it is. Only a failure the route
// settled itself (ports.ErrInjectNotDelivered) lets the pass go on; any other
// means the destination is likely down, and the next matching event tries again.
// The delete after a confirmed inject is not cancelled by a shutdown: skipping it
// would redrive the message a second time.
func (rt *Runtime) autoRedriveOne(ctx context.Context, routeID, sid string, e routing.DLQEntry) (ok, stop bool) {
	detail := map[string]any{
		"route_id":     routeID,
		"session_id":   sid,
		"subscription": e.ExtraInfo()[routing.ExtraInfoSubscription],
	}
	if err := rt.autoRedriveInject(ctx, routeID, e); err != nil {
		rt.countAutoRedrive(shared.MetricDLQRedriveFailures, routeID)
		detail["error"] = err.Error()
		rt.auditAutoRedrive(ctx, e.ID(), "failure", detail)
		if rt.logger != nil {
			rt.logger.Warn("automatic redrive failed; the DLQ record is kept",
				"dlq_id", e.ID(), "route_id", routeID, "session_id", sid, "error", err)
		}
		return false, !errors.Is(err, ports.ErrInjectNotDelivered)
	}
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoRedriveStoreTimeout)
	_, delErr := rt.dlqStore.Delete(delCtx, []string{e.ID()})
	cancel()
	rt.countAutoRedrive(shared.MetricDLQRedrives, routeID)
	if delErr != nil {
		detail["delete_error"] = delErr.Error()
		if rt.logger != nil {
			rt.logger.Warn("message redriven but DLQ record not removed; remove it to avoid a duplicate redrive",
				"dlq_id", e.ID(), "route_id", routeID, "error", delErr)
		}
	}
	rt.auditAutoRedrive(ctx, e.ID(), "success", detail)
	return true, false
}

// autoRedriveInject injects record e with hold set and turns a panic into an
// error. A synchronous inject has no per-delivery recover, so a panic in the
// route (a sender, say) would otherwise reach startBackground's recover and make
// the whole runtime terminal; as an error it keeps the record and stops the pass.
// The panic is counted as a delivery panic of the route, as the route runner
// counts one it recovers.
func (rt *Runtime) autoRedriveInject(ctx context.Context, routeID string, e routing.DLQEntry) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("runtime: automatic redrive of %q panicked: %v", e.ID(), r)
			rt.countAutoRedrive(shared.MetricDeliveryPanics, routeID)
			if rt.logger != nil {
				rt.logger.Error("automatic redrive panicked; the DLQ record is kept",
					"dlq_id", e.ID(), "route_id", routeID, "panic", r, "stack", string(goruntime.Stack()))
			}
		}
	}()
	return rt.injectRedrive(ctx, routeID, e.BindingID(), e.Snapshot(), true)
}

// countAutoRedrive emits a route-tagged counter, as the admin redrive and the
// route runner do.
func (rt *Runtime) countAutoRedrive(name, routeID string) {
	if rt.metrics != nil {
		rt.metrics.Counter(name, 1, shared.Tag{Key: shared.TagKeyRouteID, Value: routeID})
	}
}

// auditAutoRedrive logs one automatically redriven record. The audit outlives a
// shutdown that cancelled ctx: the outcome it records already happened.
func (rt *Runtime) auditAutoRedrive(ctx context.Context, id, outcome string, detail map[string]any) {
	if rt.audit == nil {
		return
	}
	rt.audit.Log(context.WithoutCancel(ctx), ports.AuditEvent{
		Timestamp:  rt.clk.Now().UTC(),
		Action:     "dlq.redrive.auto",
		Actor:      rt.instanceID,
		Resource:   "dlq",
		ResourceID: id,
		Outcome:    outcome,
		Detail:     detail,
	})
}
