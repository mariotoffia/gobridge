package httpapi

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

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
