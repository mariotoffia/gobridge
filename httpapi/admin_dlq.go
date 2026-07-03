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

// redriveTimeout bounds the detached claim→inject sequence for a full redrive
// batch so a stuck inject or store cannot hang the handler forever once the
// request context is severed from cancellation.
const redriveTimeout = 30 * time.Second

// redriveRestoreTimeout bounds a SINGLE best-effort restore of a claimed entry
// after a failed inject. It is deliberately its own short budget, freshly
// derived per restore and independent of the per-batch redriveTimeout: a batch
// that exhausts (or nearly exhausts) its budget mid-loop would otherwise fail
// the restore on the same expired context and permanently lose the claimed
// (already-deleted) entry. A fresh detached context guarantees the restore
// always gets a real chance to run.
const redriveRestoreTimeout = 10 * time.Second

// bindingInjector is the optional capability a Runtime exposes for
// binding-scoped synthetic injection. The concrete runtime implements
// InjectToBinding so the admin DLQ redrive can confine a replay to the single
// binding that failed (carried out-of-band, surviving the ingress reserved-
// header strip). The ports.Runtime contract stays minimal; adapters type-assert
// for the capability and fall back to a plain Inject when it is absent.
type bindingInjector interface {
	InjectToBinding(ctx context.Context, routeID, bindingID string, env *messaging.Envelope) error
}

// injectRedrive injects env into routeID, confining dispatch to the entry's
// binding when the runtime supports binding-scoped injection. Redriving one
// failed leg of a fan-out shared_outbox route must NOT re-persist records for
// the N-1 healthy bindings, so a non-empty bindingID takes the out-of-band
// InjectToBinding path. A header cannot carry the binding: doHandleDelivery
// strips x-bridge.route-override at ingress before any consumption site reads
// it, which is exactly the security property that keeps external messages from
// steering routing.
func injectRedrive(ctx context.Context, logger *slog.Logger, rt ports.RuntimeCommand, routeID, bindingID string, env *messaging.Envelope) error {
	if bindingID != "" {
		if bi, ok := rt.(bindingInjector); ok {
			return bi.InjectToBinding(ctx, routeID, bindingID, env)
		}
		// The entry recorded a specific failed binding, but this runtime does
		// not implement binding-scoped injection. Falling back to a plain Inject
		// fans the replay out to ALL bindings on the route, re-delivering to the
		// N-1 healthy destinations — the exact duplicate-delivery hazard the
		// binding-scoped path exists to prevent. Surface the degradation at Warn
		// so an operator can see why a supposedly-confined redrive fanned out.
		if logger != nil {
			logger.Warn("dlq redrive: runtime lacks binding-scoped injection; replay will fan out to all bindings",
				"route_id", routeID, "binding_id", bindingID)
		}
	}
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
	entries, err := store.List(r.Context(), routing.DLQFilter{Limit: dlqSummaryCap})
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

	entries, err := store.List(r.Context(), filter)
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
	entry, err := store.Get(r.Context(), id)
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

	// Detach the claim→inject→restore sequence from the request context so an
	// operator disconnect mid-batch cannot cancel an in-flight restore and
	// permanently lose a claimed (already-deleted) entry — the claim-by-delete
	// below makes that loss irreversible. A bounded timeout still caps a stuck
	// inject or store call.
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), redriveTimeout)
	defer cancel()

	seen := make(map[string]struct{}, len(body.IDs))
	for _, id := range body.IDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}

		entry, err := reader.Get(opCtx, id)
		if err != nil {
			redriveErrors = append(redriveErrors, redriveError{
				ID: id, Error: "entry not found",
			})
			continue
		}

		// Claim-by-delete BEFORE inject. Deleting first makes redrive safe
		// under (a) client retries — a re-sent request finds the entry already
		// gone (count==0) and skips it instead of re-injecting, and (b)
		// concurrent admin instances — only the instance whose Delete returns 1
		// owns the entry and injects it. This trades an at-most-once window (a
		// crash between delete and inject drops the entry) for no double
		// delivery, which is the correct bias for a manual redrive; the inject
		// failure path below best-effort restores the entry to close most of
		// that window.
		deleted, err := admin.Delete(opCtx, []string{id})
		if err != nil {
			redriveErrors = append(redriveErrors, redriveError{
				ID: id, Error: "failed to claim entry for redrive",
			})
			continue
		}
		if deleted == 0 {
			// Lost the race (or a retry): another actor already claimed it.
			redriveErrors = append(redriveErrors, redriveError{
				ID: id, Error: "entry already redriven or concurrently deleted",
			})
			continue
		}

		// Binding-scoped dispatch: the entry records the exact BindingID that
		// failed. injectRedrive carries that binding out-of-band via
		// Runtime.InjectToBinding (NOT a header — the ingress reserved-header
		// strip in doHandleDelivery removes any x-bridge.route-override before a
		// consumption site reads it), confining the replay to that one binding so
		// the N-1 healthy bindings on a fan-out route do not receive duplicate
		// deliveries. Snapshot returns a fresh deep copy.
		env := entry.Snapshot()
		if err := injectRedrive(opCtx, s.logger, rt, entry.RouteID(), entry.BindingID(), env); err != nil {
			msg := "inject failed"
			if errors.Is(err, shared.ErrNotFound) {
				msg = "route not found"
			}
			// Inject failed after we claimed the entry: best-effort restore so
			// the failure evidence is not lost. The restore runs under its OWN
			// fresh detached context (context.WithoutCancel + a short timeout),
			// NOT the per-batch opCtx: a batch that exhausted its budget mid-loop
			// would otherwise fail this restore on the same expired context and
			// permanently drop the claimed entry. If restore still fails, flag
			// the entry as dropped so the operator can investigate.
			restoreCtx, restoreCancel := context.WithTimeout(context.WithoutCancel(r.Context()), redriveRestoreTimeout)
			restoreErr := admin.Write(restoreCtx, entry)
			restoreCancel()
			if restoreErr != nil {
				msg += " (entry lost: restore failed)"
			}
			redriveErrors = append(redriveErrors, redriveError{
				ID: id, Error: msg,
			})
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

	// 207 Multi-Status when any entry failed (the caller must inspect the
	// per-entry errors array); 200 only when every requested entry redrove.
	status := http.StatusOK
	if len(redriveErrors) > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, resp)
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

	n, err := store.Delete(r.Context(), body.IDs)
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

	// Safety guard: require confirmation for unfiltered delete-all
	hasFilter := filter.RouteID != "" || filter.Category != "" ||
		!filter.Since.IsZero() || !filter.Before.IsZero() || filter.Limit > 0
	if !hasFilter && !body.ConfirmDeleteAll {
		writeErr(w, http.StatusBadRequest,
			"empty filter would delete all entries; set confirm_delete_all=true to proceed")
		return
	}

	n, err := store.DeleteByFilter(r.Context(), filter)
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
	count, err := store.Purge(r.Context(), s.clk.Now().UTC())
	if err != nil {
		s.emitAudit(r, "dlq.purge", "dlq", "", "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusInternalServerError, "DLQ purge failed")
		return
	}
	s.emitAudit(r, "dlq.purge", "dlq", "", "success", map[string]any{"purged": count})
	writeJSON(w, http.StatusOK, map[string]int{"purged": count})
}
