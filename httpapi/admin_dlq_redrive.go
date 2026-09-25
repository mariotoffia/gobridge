package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// redriveTimeout bounds the detached inject→delete sequence for a full redrive
// batch so a stuck inject or store cannot hang the handler forever once the
// request context is severed from cancellation.
const redriveTimeout = 30 * time.Second

// redriveEntryTimeout bounds one entry's lookup and inject inside the batch, so
// an entry whose destination is down cannot spend the whole batch budget and
// leave every later id unattempted. The effective bound is the smaller of this
// and what remains of the batch.
const redriveEntryTimeout = 10 * time.Second

// redriveInjector is the optional capability a Runtime exposes for
// DLQ-redrive-safe injection: the message is re-issued under a FRESH envelope
// ID with the original ID stamped as provenance (x-bridge.causation-id).
// Reusing the original ID is a verified silent-loss path on shared_outbox
// routes: the outbox retains completed/poisoned rows as dedup evidence keyed
// on (envelope_id, binding_id), so re-persisting the same ID returns
// duplicate → the dispatch ACKs → the redrive reports success → the DLQ entry
// is deleted — and the message is never sent again. Adapters type-assert for
// this capability and REFUSE to redrive a binding-scoped (shared_outbox) entry
// when it is absent (see injectRedrive).
type redriveInjector interface {
	InjectRedrive(ctx context.Context, routeID, bindingID string, env *messaging.Envelope) error
}

// errRedriveUnsafeSharedOutbox is returned by injectRedrive when the runtime
// lacks redrive-safe injection (InjectRedrive) AND the entry targets a specific
// binding — a shared_outbox fan-out leg. Replaying such an entry through any
// legacy path (InjectToBinding or plain Inject) reuses the ORIGINAL envelope
// ID, which the outbox's retained UNIQUE(envelope_id, binding_id) dedup row
// swallows as a duplicate: the dispatch ACKs, the redrive would report success,
// and the DLQ entry would be deleted while nothing is actually re-sent — silent
// loss of BOTH the message and its failure evidence. The redrive is refused
// (no inject, no delete) so the entry and its evidence are preserved. Upgrade
// path: a runtime implementing InjectRedrive (fresh ID + causation provenance),
// which the concrete runtime.Runtime does.
var errRedriveUnsafeSharedOutbox = errors.New("refusing redrive: runtime lacks redrive-safe injection and this entry targets a shared_outbox binding")

// errRedriveUnsafeNoFreshID is returned by injectRedrive when the runtime lacks
// redrive-safe injection (InjectRedrive) AND a DIRECT (non-binding) entry could
// still be silently deduplicated by an idempotent/FIFO transport: the legacy
// plain-Inject replay would reuse the ORIGINAL envelope ID and/or its
// x-bridge.dedup-id header, and a sender that dedups on either (e.g. SQS FIFO
// maps x-bridge.dedup-id → MessageDeduplicationId, else hashes the envelope ID)
// would ACK WITHOUT delivering → Send returns nil → the redrive would report
// success and the DLQ entry would be deleted after a no-op. That is the same
// silent evidence loss as the shared_outbox path, just outside shared_outbox.
// The redrive is refused (no inject, no delete). Only a collision-free direct
// entry — EMPTY envelope ID AND no x-bridge.dedup-id header — is safe for the
// legacy path (injectToBinding then assigns a fresh ID). Upgrade path: a runtime
// implementing InjectRedrive (fresh ID + stripped dedup key), which the concrete
// runtime.Runtime does.
var errRedriveUnsafeNoFreshID = errors.New("refusing redrive: runtime lacks redrive-safe injection and this entry carries a dedup-prone identity")

// injectRedrive injects env into routeID for a DLQ redrive.
//
// When the runtime supports redrive-safe injection (InjectRedrive) the replay
// is re-issued under a FRESH envelope ID with the original stamped as
// provenance, and dispatch is confined out-of-band to the entry's binding
// (redriving one failed leg of a fan-out shared_outbox route must NOT re-deliver
// to the N-1 healthy bindings). A header cannot carry the binding: doHandleDelivery
// strips x-bridge.route-override at ingress before any consumption site reads
// it, which is the security property that keeps external messages from steering
// routing.
//
// When the runtime LACKS redrive-safe injection the replay would reuse the
// original envelope ID. For a binding-scoped entry (a shared_outbox fan-out leg)
// that is a proven silent-loss path — the outbox's retained dedup row swallows
// the re-persist as a duplicate — so it is REFUSED with
// errRedriveUnsafeSharedOutbox (the caller must NOT delete the entry). A direct
// entry (empty bindingID) has no shared_outbox dedup row, but an idempotent/FIFO
// transport can still swallow a replay that reuses the original envelope ID or
// its x-bridge.dedup-id header, so a direct entry is refused with
// errRedriveUnsafeNoFreshID UNLESS it is collision-free (empty ID AND no dedup
// header); only then does a plain Inject stay safe on runtimes that predate
// InjectRedrive.
func injectRedrive(ctx context.Context, logger *slog.Logger, rt ports.RuntimeCommand, routeID, bindingID string, env *messaging.Envelope) error {
	if ri, ok := rt.(redriveInjector); ok {
		return ri.InjectRedrive(ctx, routeID, bindingID, env)
	}
	if bindingID != "" {
		// Binding-scoped entries are exactly the shared_outbox fan-out legs
		// where reusing the original envelope ID is a proven silent-loss path.
		// Even a binding-confined InjectToBinding would reuse that ID and be
		// swallowed by the outbox dedup row, so REFUSE the redrive rather than
		// inject-and-delete. The entry and its failure evidence are preserved.
		if logger != nil {
			logger.Warn("dlq redrive: refusing shared_outbox/binding entry; runtime lacks redrive-safe injection (InjectRedrive), so a replay would reuse the original envelope id and risk silent outbox dedup loss",
				"route_id", routeID, "binding_id", bindingID)
		}
		return errRedriveUnsafeSharedOutbox
	}
	// Direct entry (no binding): there is no shared_outbox dedup row to collide
	// with, but an idempotent/FIFO transport can STILL silently deduplicate a
	// replay that reuses the original envelope ID or its x-bridge.dedup-id header
	// (e.g. SQS FIFO maps x-bridge.dedup-id → MessageDeduplicationId, else hashes
	// the envelope ID). A dedup hit ACKs WITHOUT delivering, Send returns nil, and
	// the caller would delete the entry after a no-op. So a direct entry is only
	// safe for the legacy plain-Inject path when it is collision-free: EMPTY ID
	// AND no dedup-id header (injectToBinding then assigns a FRESH ID). Anything
	// else is refused so the entry and its evidence are preserved.
	if env.ID() != "" {
		if logger != nil {
			logger.Warn("dlq redrive: refusing direct entry with a non-empty envelope id; runtime lacks redrive-safe injection (InjectRedrive), so a replay would reuse the original id and risk silent transport dedup loss",
				"route_id", routeID, "envelope_id", env.ID())
		}
		return errRedriveUnsafeNoFreshID
	}
	if _, hasDedup := env.Header(messaging.HeaderDeduplicationID); hasDedup {
		if logger != nil {
			logger.Warn("dlq redrive: refusing direct entry carrying x-bridge.dedup-id; runtime lacks redrive-safe injection (InjectRedrive), so a replay would reuse the dedup key and risk silent transport dedup loss",
				"route_id", routeID)
		}
		return errRedriveUnsafeNoFreshID
	}
	// Collision-free direct entry (empty ID, no dedup key): a plain Inject is
	// safe — the runtime assigns a fresh ID and the transport re-derives dedup
	// from it. The response still surfaces a verify-delivery warning (see
	// handleDLQRedrive) because the runtime is not redrive-safe.
	return rt.Inject(ctx, routeID, env)
}

func (s *Server) handleDLQRedrive(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	reader := rt.DLQReader()
	if reader == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}
	// Nil-check the admin (write) side up front: redrive both injects AND
	// deletes, so a runtime with a read-only DLQ must fail before any inject
	// happens rather than panicking on a nil DLQAdmin mid-flight.
	admin := rt.DLQAdmin()
	if admin == nil {
		writeErr(w, http.StatusNotFound, "no DLQ admin store configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := decodeStrictJSON(r.Body, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "ids must not be empty")
		return
	}
	if len(body.IDs) > maxRedriveIDs {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("ids exceeds maximum of %d", maxRedriveIDs))
		return
	}

	type redriveError struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}

	var successIDs []string
	var redriveErrors []redriveError

	// Redrive-safe injection (fresh envelope ID + provenance) avoids the
	// shared_outbox dedup silent-loss path. When the runtime lacks it the replay
	// reuses the original ID and a completed/poisoned outbox row can swallow the
	// re-persist while this handler still reports the entry redriven — so a
	// non-fatal warning is surfaced in the response (see below).
	_, redriveSafe := rt.(redriveInjector)

	// Detach the inject→delete sequence from the request context so an operator
	// disconnect mid-batch cannot cancel an in-flight delete that follows a
	// successful inject — a cancelled delete would leave the (already-delivered)
	// entry behind and cause a duplicate redrive on the next attempt. Two bounds
	// still cap a stuck inject or store call: s.redriveTimeout caps the whole
	// batch, and s.redriveEntryTimeout caps each entry's lookup and inject (see
	// redriveOne), so one entry whose destination is down cannot spend the budget
	// of the ids after it.
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.redriveTimeout)
	defer cancel()

	// Emit an intent record BEFORE the inject→delete loop so a crash between a
	// successful Inject and its Delete leaves an audit trace of which entry IDs
	// were being redriven (the per-batch outcome record below only exists if the
	// handler returns).
	s.emitAudit(r, "dlq.redrive.begin", "dlq", "", "pending", map[string]any{"ids": body.IDs})

	seen := make(map[string]struct{}, len(body.IDs))
	for _, id := range body.IDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}

		if ok, errMsg := s.redriveOne(opCtx, reader, admin, rt, id); !ok {
			redriveErrors = append(redriveErrors, redriveError{ID: id, Error: errMsg})
			continue
		}
		successIDs = append(successIDs, id)
	}

	outcome := "success"
	if len(redriveErrors) > 0 {
		outcome = "partial_failure"
	}
	failedIDs := make([]string, len(redriveErrors))
	for i := range redriveErrors {
		failedIDs[i] = redriveErrors[i].ID
	}
	s.emitAudit(r, "dlq.redrive", "dlq", "", outcome, map[string]any{
		"redriven":   len(successIDs),
		"failed":     len(redriveErrors),
		"ids":        body.IDs,
		"failed_ids": failedIDs,
	})

	resp := map[string]any{
		"redriven": len(successIDs),
		"failed":   len(redriveErrors),
	}
	if len(redriveErrors) > 0 {
		resp["errors"] = redriveErrors
	}
	// Flag the silent-loss hazard: without redrive-safe injection a replay that
	// reused the original envelope ID may have been swallowed by outbox dedup on
	// a shared_outbox route, so a reported "redriven" is NOT proof of delivery.
	// The redrive is not failed (the claim+inject completed), but the operator
	// must verify — a bare 200 would hide the no-op.
	if !redriveSafe && len(successIDs) > 0 {
		resp["warning"] = "runtime lacks redrive-safe injection: replays reuse the original envelope id and may be silently deduplicated by the outbox on shared_outbox routes; verify delivery"
	}

	// 207 Multi-Status when any entry failed (the caller must inspect the
	// per-entry errors array); 200 only when every requested entry redrove.
	status := http.StatusOK
	if len(redriveErrors) > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, resp)
}

// redriveOne looks up, injects and removes one DLQ entry. It reports ok, or the
// per-entry error the response carries for id. The lookup and inject run under
// s.redriveEntryTimeout, derived from the batch context opCtx, so the effective
// bound is the smaller of the two. The delete runs under opCtx alone: a confirmed
// inject's delete must not lose a race to the entry bound, or the delivered entry
// stays behind and the next redrive duplicates it.
func (s *Server) redriveOne(opCtx context.Context, reader ports.DLQReader, admin ports.DLQAdmin, rt ports.RuntimeCommand, id string) (bool, string) {
	entryCtx, cancelEntry := context.WithTimeout(opCtx, s.redriveEntryTimeout)
	defer cancelEntry()

	entry, err := reader.Get(entryCtx, id)
	if err != nil {
		// An expired entry or batch bound surfaces here as a Get error too; label
		// it honestly instead of the misleading "entry not found" so a lookup that
		// ran out of budget is not mistaken for a missing entry.
		if entryCtx.Err() != nil {
			return false, "redrive deadline exceeded before entry lookup"
		}
		return false, "entry not found"
	}

	// Inject BEFORE delete (at-least-once). The previous claim-by-delete
	// ordering deleted the entry FIRST and injected afterwards, so a crash /
	// SIGKILL / store outage between Delete and Inject lost BOTH the message
	// and its DLQ evidence — an irreversible at-most-once window
	// (dlq.redrive.begin records IDs, not recoverable payloads). Injecting
	// first and deleting only after a CONFIRMED inject makes a failed inject
	// leave the entry fully intact: no loss. The cost is a bounded duplicate
	// window — a crash between a successful Inject and the Delete re-drives
	// on the next attempt (at-least-once). For a manual recovery action,
	// never losing the message is the correct bias.
	//
	// Binding-scoped dispatch: the entry records the exact BindingID that
	// failed. When the runtime supports redrive-safe injection, injectRedrive
	// re-issues under a FRESH envelope ID and carries that binding out-of-band
	// via Runtime.InjectRedrive (NOT a header — the ingress reserved-header
	// strip in doHandleDelivery removes any x-bridge.route-override before a
	// consumption site reads it), confining the replay to that one binding so
	// the N-1 healthy bindings on a fan-out route do not receive duplicate
	// deliveries. When the runtime LACKS redrive-safe injection a
	// binding-scoped entry is REFUSED (errRedriveUnsafeSharedOutbox) and a
	// dedup-prone direct entry (non-empty ID or an x-bridge.dedup-id header) is
	// REFUSED (errRedriveUnsafeNoFreshID): the original-ID/dedup-key replay
	// would be swallowed by outbox or transport dedup and silently lost, so the
	// entry is left intact rather than deleted after a no-op. Snapshot returns a
	// fresh deep copy.
	env := entry.Snapshot()
	if err := injectRedrive(entryCtx, s.logger, rt, entry.RouteID(), entry.BindingID(), env); err != nil {
		// Carry the cause: a redrive can fail because the route DROPPED or
		// re-DLQ'd the replay (the runtime reports a terminal settle that
		// delivered nothing), and "inject failed" alone leaves the operator
		// with no way to tell that from a missing route.
		msg := "inject failed: " + err.Error()
		switch {
		case errors.Is(err, errRedriveUnsafeSharedOutbox):
			// The runtime cannot confirm a non-duplicate enqueue for this
			// shared_outbox/binding entry, so the redrive was refused BEFORE
			// any inject. The entry is intact; the message and its evidence
			// are preserved. The operator must upgrade the runtime to one
			// that implements redrive-safe injection (InjectRedrive).
			msg = "refused: runtime lacks redrive-safe injection; redriving this shared_outbox/binding entry would reuse the original envelope id and risk silent outbox dedup loss — entry preserved (no delete)"
		case errors.Is(err, errRedriveUnsafeNoFreshID):
			// A DIRECT entry that carries a non-empty ID or a dedup-id header
			// was refused BEFORE any inject: an idempotent/FIFO transport could
			// silently swallow the original-ID/dedup-key replay, so the entry
			// is left intact. The operator must upgrade the runtime to one that
			// implements redrive-safe injection (InjectRedrive).
			msg = "refused: runtime lacks redrive-safe injection; redriving this entry would reuse its original envelope id or dedup key and risk silent transport dedup loss — entry preserved (no delete)"
		case errors.Is(err, shared.ErrNotFound):
			// ErrNotFound from redrive now most often means the recorded
			// binding no longer exists on a still-present (reconfigured)
			// route, not that the route itself is gone.
			msg = "route or binding not found"
		}
		// Inject failed or was refused: the entry was NEVER deleted, so both
		// the failure evidence and the message survive in the DLQ. A
		// route-tagged failure counter lets an operator alert on
		// manual-recovery churn the batch-level audit record does not
		// surface. No-op when no metrics exporter is wired.
		s.countRedrive(shared.MetricDLQRedriveFailures, entry.RouteID())
		return false, msg
	}

	// Inject confirmed: only NOW remove the entry. A failed delete means the
	// message WAS delivered but the entry lingers, so a later redrive
	// re-delivers (a bounded at-least-once duplicate, NOT a loss). Surface it
	// so the operator removes the entry manually rather than believing the
	// redrive failed. A deleted count of 0 (a concurrent redrive already
	// removed it) is benign — our inject still happened.
	if _, err := admin.Delete(opCtx, []string{id}); err != nil {
		s.countRedrive(shared.MetricDLQRedrives, entry.RouteID())
		return false, "message re-injected but DLQ entry not removed (delete failed); remove it manually to avoid a duplicate redrive"
	}

	s.countRedrive(shared.MetricDLQRedrives, entry.RouteID())
	return true, ""
}

// countRedrive emits a route-tagged redrive counter when a metrics exporter is
// configured. A nil exporter (the default) makes it a no-op, so callers need no
// guard. name is shared.MetricDLQRedrives (entry redriven) or
// shared.MetricDLQRedriveFailures (claim ok but inject failed).
func (s *Server) countRedrive(name, routeID string) {
	if s.metrics == nil {
		return
	}
	s.metrics.Counter(name, 1, shared.Tag{Key: shared.TagKeyRouteID, Value: routeID})
}
