package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// redriveTimeout bounds the detached inject→delete sequence for a full redrive
// batch so a stuck inject or store cannot hang the handler forever once the
// request context is severed from cancellation.
const redriveTimeout = 30 * time.Second

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

// dlqEntryView is the HTTP-layer representation of a DLQ entry.
// It uses snake_case JSON tags consistent with the rest of the API.
type dlqEntryView struct {
	ID            string    `json:"id"`
	RouteID       string    `json:"route_id"`
	BindingID     string    `json:"binding_id"`
	SessionID     string    `json:"session_id"`
	SourceID      string    `json:"source_id"`
	CorrelationID string    `json:"correlation_id"`
	Subject       string    `json:"subject"`
	Reason        string    `json:"reason"`
	Category      string    `json:"category"`
	ErrorCode     string    `json:"error_code"`
	LastError     string    `json:"last_error"`
	FailedAt      time.Time `json:"failed_at"`
	Attempts      int       `json:"attempts"`
}

// dlqEntryDetailView extends dlqEntryView with the envelope payload
// for single-entry GET responses.
type dlqEntryDetailView struct {
	dlqEntryView
	Payload string `json:"payload"` // base64-encoded
}

func toDLQEntryView(e routing.DLQEntry) dlqEntryView {
	return dlqEntryView{
		ID:            e.ID(),
		RouteID:       e.RouteID(),
		BindingID:     e.BindingID(),
		SessionID:     e.SessionID(),
		SourceID:      e.SourceID(),
		CorrelationID: e.CorrelationID(),
		Subject:       e.Snapshot().Subject(),
		Reason:        e.Reason(),
		Category:      e.Category(),
		ErrorCode:     e.ErrorCode(),
		LastError:     e.LastError(),
		FailedAt:      e.FailedAt(),
		Attempts:      e.Attempts(),
	}
}

func toDLQEntryViews(entries []routing.DLQEntry) []dlqEntryView {
	views := make([]dlqEntryView, len(entries))
	for i, e := range entries {
		views[i] = toDLQEntryView(e)
	}
	return views
}

func toDLQEntryDetailView(e routing.DLQEntry) dlqEntryDetailView {
	return dlqEntryDetailView{
		dlqEntryView: toDLQEntryView(e),
		Payload:      base64.StdEncoding.EncodeToString(e.Snapshot().Payload()),
	}
}

const (
	defaultDLQLimit = 100
	maxDLQLimit     = 1000
	// maxDLQOffset bounds pagination offset so a caller cannot force the store
	// to materialize an unbounded prefix (offset+limit) under its lock, and so
	// offset+limit cannot overflow int.
	maxDLQOffset  = 100_000
	maxRedriveIDs = 100
	maxDeleteIDs  = 1000
	// maxDeleteByFilterLimit caps a POSITIVE delete-by-filter limit so a
	// confirmed delete still cannot launch an effectively unbounded destructive
	// scan via an absurd bound (e.g. {"limit":2147483647} would delete the whole
	// DLQ). A caller who genuinely wants "delete every matching entry" uses
	// limit==0 (unbounded WITHIN the filter) — which the confirm_delete_all guard
	// gates when the filter is otherwise empty — not a giant positive number.
	// ponytail: fixed ceiling. If a deployment needs a larger single-call bounded
	// delete, raise this cap or page via repeated calls / limit==0 + a filter.
	maxDeleteByFilterLimit = 10_000
	// dlqSummaryCap bounds the entries scanned for the /dlq summary count; when
	// hit, the response flags count_capped so operators know depth exceeds it.
	dlqSummaryCap = maxDLQLimit
)

func (s *Server) handleDLQ(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	store := rt.DLQReader()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}
	// Bound the store scan so a wedged backend cannot hang the handler.
	opCtx, cancel := s.adminOpContext(r.Context())
	defer cancel()
	entries, err := store.List(opCtx, routing.DLQFilter{Limit: dlqSummaryCap})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list DLQ entries")
		return
	}
	// count reflects entries scanned up to dlqSummaryCap. Without a Count port
	// the true depth is unknown when the cap is hit; count_capped tells the
	// operator the real backlog is at least this large so alerting is not
	// silently clamped.
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":   true,
		"count":        len(entries),
		"count_capped": len(entries) >= dlqSummaryCap,
	})
}

func (s *Server) handleDLQMessages(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	store := rt.DLQReader()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}

	q := r.URL.Query()
	limit := defaultDLQLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > maxDLQLimit {
			n = maxDLQLimit
		}
		limit = n
	}

	offset := 0
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		if n > maxDLQOffset {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("offset exceeds maximum of %d", maxDLQOffset))
			return
		}
		offset = n
	}

	// Fetch offset+limit+1 (bounded: both are capped, so no int overflow). The
	// extra +1 lets us detect whether a further page exists without a Count
	// port, so has_more is truthful (the old `total` reported min(matched,
	// limit+offset), which lied once the backlog exceeded the page window).
	filter := routing.DLQFilter{
		RouteID:  q.Get("route_id"),
		Category: q.Get("category"),
		Limit:    offset + limit + 1,
	}

	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "since must be RFC3339 format")
			return
		}
		filter.Since = t
	}
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "before must be RFC3339 format")
			return
		}
		filter.Before = t
	}

	// Bound the store scan so a wedged backend cannot hang the handler.
	opCtx, cancel := s.adminOpContext(r.Context())
	defer cancel()
	entries, err := store.List(opCtx, filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list DLQ messages")
		return
	}

	// Apply offset.
	if offset >= len(entries) {
		entries = nil
	} else {
		entries = entries[offset:]
	}
	// Detect and trim to the page; a surplus beyond limit means more pages.
	hasMore := false
	if len(entries) > limit {
		hasMore = true
		entries = entries[:limit]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"messages": toDLQEntryViews(entries),
		"limit":    limit,
		"offset":   offset,
		"has_more": hasMore,
	})
}

func (s *Server) handleDLQMessageByID(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	store := rt.DLQReader()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}

	id := r.PathValue("id")
	// Bound the store lookup so a wedged backend cannot hang the handler.
	opCtx, cancel := s.adminOpContext(r.Context())
	defer cancel()
	entry, err := store.Get(opCtx, id)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "DLQ entry not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to get DLQ entry")
		return
	}

	// Reading a single entry returns its full (base64) payload, which can carry
	// PII/secrets. Config reads are audited; this equally sensitive read must
	// be too, so payload disclosure is attributable.
	s.emitAudit(r, "dlq.read_payload", "dlq", id, "success", nil)

	writeJSON(w, http.StatusOK, toDLQEntryDetailView(entry))
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
	// entry behind and cause a duplicate redrive on the next attempt. A bounded
	// timeout still caps a stuck inject or store call.
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), redriveTimeout)
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

		entry, err := reader.Get(opCtx, id)
		if err != nil {
			// A severed/expired opCtx surfaces here as a Get error too; label it
			// honestly instead of the misleading "entry not found" so a batch
			// that ran out of budget is not mistaken for missing entries.
			errMsg := "entry not found"
			if opCtx.Err() != nil {
				errMsg = "redrive deadline exceeded before entry lookup"
			}
			redriveErrors = append(redriveErrors, redriveError{
				ID: id, Error: errMsg,
			})
			continue
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
		if err := injectRedrive(opCtx, s.logger, rt, entry.RouteID(), entry.BindingID(), env); err != nil {
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
			redriveErrors = append(redriveErrors, redriveError{
				ID: id, Error: msg,
			})
			continue
		}

		// Inject confirmed: only NOW remove the entry. A failed delete means the
		// message WAS delivered but the entry lingers, so a later redrive
		// re-delivers (a bounded at-least-once duplicate, NOT a loss). Surface it
		// so the operator removes the entry manually rather than believing the
		// redrive failed. A deleted count of 0 (a concurrent redrive already
		// removed it) is benign — our inject still happened.
		if _, err := admin.Delete(opCtx, []string{id}); err != nil {
			s.countRedrive(shared.MetricDLQRedrives, entry.RouteID())
			redriveErrors = append(redriveErrors, redriveError{
				ID:    id,
				Error: "message re-injected but DLQ entry not removed (delete failed); remove it manually to avoid a duplicate redrive",
			})
			continue
		}

		s.countRedrive(shared.MetricDLQRedrives, entry.RouteID())
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

func (s *Server) handleDLQDeleteByIDs(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	store := rt.DLQAdmin()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
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
	if len(body.IDs) > maxDeleteIDs {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("ids exceeds maximum of %d", maxDeleteIDs))
		return
	}

	// Bound the backend delete so a wedged store cannot hang the handler.
	opCtx, cancel := s.adminOpContext(r.Context())
	defer cancel()
	n, err := store.Delete(opCtx, body.IDs)
	if err != nil {
		s.emitAudit(r, "dlq.delete", "dlq", "", "failure", map[string]any{
			"ids":   body.IDs,
			"error": err.Error(),
		})
		writeErr(w, http.StatusInternalServerError, "DLQ delete failed")
		return
	}

	s.emitAudit(r, "dlq.delete", "dlq", "", "success", map[string]any{
		"deleted": n,
		"ids":     body.IDs,
	})
	writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

func (s *Server) handleDLQDeleteByFilter(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	store := rt.DLQAdmin()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		RouteID          string `json:"route_id"`
		Category         string `json:"category"`
		Since            string `json:"since"`
		Before           string `json:"before"`
		Limit            int    `json:"limit"`
		ConfirmDeleteAll bool   `json:"confirm_delete_all"`
	}
	if err := decodeStrictJSON(r.Body, &body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// A NEGATIVE limit is meaningless as a bound and DANGEROUS: the DeleteByFilter
	// port contract (ports.DLQAdmin) treats DLQFilter.Limit <= 0 as "delete EVERY
	// matching entry". Copying a caller-supplied negative straight into the filter
	// would turn a request like {"route_id":"r","limit":-1} — which reads as a
	// bounded delete of one — into an UNBOUNDED destructive delete of all matching
	// DLQ evidence. Reject it before the filter is built.
	if body.Limit < 0 {
		writeErr(w, http.StatusBadRequest, "limit must not be negative")
		return
	}
	// A huge POSITIVE limit is the mirror hazard. A limit is a BOUND, not a
	// content selector, so a bare {"limit":2147483647} with no route/category/time
	// predicate is still a delete-all (the hasFilter guard below no longer treats
	// a limit as a filter) and, even WITH a filter, a giant bound would let a
	// confirmed delete launch an effectively unbounded destructive scan. Cap it so
	// a positive limit stays a genuine bound.
	if body.Limit > maxDeleteByFilterLimit {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("limit exceeds maximum of %d", maxDeleteByFilterLimit))
		return
	}
	// Limit == 0 (the omitted-field default) is DELIBERATELY kept as "unbounded
	// within the provided filter" per the port contract — the common "delete all
	// entries for route X" case. The hasFilter/confirm_delete_all guard below keeps
	// a limit==0 (or any bare-limit) request with NO other filter unambiguous: it
	// demands an explicit confirm_delete_all before wiping the whole DLQ.

	filter := routing.DLQFilter{
		RouteID:  body.RouteID,
		Category: body.Category,
		Limit:    body.Limit,
	}

	if body.Since != "" {
		t, err := time.Parse(time.RFC3339, body.Since)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "since must be RFC3339 format")
			return
		}
		filter.Since = t
	}
	if body.Before != "" {
		t, err := time.Parse(time.RFC3339, body.Before)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "before must be RFC3339 format")
			return
		}
		filter.Before = t
	}

	// Safety guard: require confirmation for an unconfirmed delete-all. A limit is
	// a BOUND, not a content selector, so a bare {"limit":N} (or the limit==0
	// default) with no route/category/time predicate is still a delete-all and
	// MUST carry confirm_delete_all — a positive limit alone no longer satisfies
	// hasFilter (previously `|| filter.Limit > 0` let {"limit":2147483647} bypass
	// this confirmation and wipe the whole DLQ).
	hasFilter := filter.RouteID != "" || filter.Category != "" ||
		!filter.Since.IsZero() || !filter.Before.IsZero()
	if !hasFilter && !body.ConfirmDeleteAll {
		writeErr(w, http.StatusBadRequest,
			"empty filter would delete all entries; set confirm_delete_all=true to proceed")
		return
	}

	// Bound the backend delete so a wedged store cannot hang the handler.
	opCtx, cancel := s.adminOpContext(r.Context())
	defer cancel()
	n, err := store.DeleteByFilter(opCtx, filter)
	if err != nil {
		s.emitAudit(r, "dlq.delete_by_filter", "dlq", "", "failure", map[string]any{
			"error": err.Error(),
		})
		writeErr(w, http.StatusInternalServerError, "DLQ delete by filter failed")
		return
	}

	s.emitAudit(r, "dlq.delete_by_filter", "dlq", "", "success", map[string]any{
		"deleted":  n,
		"route_id": body.RouteID,
		"category": body.Category,
	})
	writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

func (s *Server) handleDLQPurge(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	store := rt.DLQAdmin()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		ConfirmPurgeAll bool `json:"confirm_purge_all"`
	}
	if err := decodeStrictJSON(r.Body, &body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Purge destroys the ENTIRE DLQ (all failure evidence) unconditionally.
	// Mirror delete-by-filter's confirm_delete_all guard so a single mistyped
	// path cannot wipe the queue: require explicit confirm_purge_all=true.
	if !body.ConfirmPurgeAll {
		writeErr(w, http.StatusBadRequest,
			"purge deletes the entire DLQ; set confirm_purge_all=true to proceed")
		return
	}
	// Bound the backend purge so a wedged store cannot hang the handler.
	opCtx, cancel := s.adminOpContext(r.Context())
	defer cancel()
	count, err := store.Purge(opCtx, s.clk.Now().UTC())
	if err != nil {
		s.emitAudit(r, "dlq.purge", "dlq", "", "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusInternalServerError, "DLQ purge failed")
		return
	}
	s.emitAudit(r, "dlq.purge", "dlq", "", "success", map[string]any{"purged": count})
	writeJSON(w, http.StatusOK, map[string]int{"purged": count})
}
