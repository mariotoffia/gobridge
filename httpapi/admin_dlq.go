package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mariotoffia/gobridge/domain"
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

func toDLQEntryView(e domain.DLQEntry) dlqEntryView {
	return dlqEntryView{
		ID:            e.ID,
		RouteID:       e.RouteID,
		BindingID:     e.BindingID,
		SessionID:     e.SessionID,
		SourceID:      e.SourceID,
		CorrelationID: e.CorrelationID,
		Subject:       e.Envelope.Subject,
		Reason:        e.Reason,
		Category:      e.Category,
		ErrorCode:     e.ErrorCode,
		LastError:     e.LastError,
		FailedAt:      e.FailedAt,
		Attempts:      e.Attempts,
	}
}

func toDLQEntryViews(entries []domain.DLQEntry) []dlqEntryView {
	views := make([]dlqEntryView, len(entries))
	for i, e := range entries {
		views[i] = toDLQEntryView(e)
	}
	return views
}

func toDLQEntryDetailView(e domain.DLQEntry) dlqEntryDetailView {
	return dlqEntryDetailView{
		dlqEntryView: toDLQEntryView(e),
		Payload:      base64.StdEncoding.EncodeToString(e.Envelope.Payload),
	}
}

const (
	defaultDLQLimit = 100
	maxDLQLimit     = 1000
	maxRedriveIDs   = 100
	maxDeleteIDs    = 1000
)

func (s *Server) handleDLQ(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	store := rt.DLQStore()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}
	entries, err := store.List(r.Context(), domain.DLQFilter{Limit: 100})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list DLQ entries")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"count":      len(entries),
	})
}

func (s *Server) handleDLQMessages(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	store := rt.DLQStore()
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
		offset = n
	}

	filter := domain.DLQFilter{
		RouteID:  q.Get("route_id"),
		Category: q.Get("category"),
		Limit:    limit + offset, // fetch enough to slice
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

	total := len(entries)

	// Apply offset
	if offset > len(entries) {
		entries = nil
	} else if offset > 0 {
		entries = entries[offset:]
	}
	// Apply limit
	if len(entries) > limit {
		entries = entries[:limit]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"messages": toDLQEntryViews(entries),
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	})
}

func (s *Server) handleDLQMessageByID(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	store := rt.DLQStore()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}

	id := r.PathValue("id")
	entry, err := store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "DLQ entry not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to get DLQ entry")
		return
	}

	writeJSON(w, http.StatusOK, toDLQEntryDetailView(entry))
}

func (s *Server) handleDLQRedrive(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	store := rt.DLQStore()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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

	seen := make(map[string]struct{}, len(body.IDs))
	for _, id := range body.IDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		entry, err := store.Get(r.Context(), id)
		if err != nil {
			redriveErrors = append(redriveErrors, redriveError{
				ID: id, Error: "entry not found",
			})
			continue
		}

		if err := rt.Inject(r.Context(), entry.RouteID, &entry.Envelope); err != nil {
			msg := "inject failed"
			if errors.Is(err, domain.ErrNotFound) {
				msg = "route not found"
			}
			redriveErrors = append(redriveErrors, redriveError{
				ID: id, Error: msg,
			})
			continue
		}

		successIDs = append(successIDs, id)
	}

	// Delete successfully redriven entries
	var deleteErr string
	if len(successIDs) > 0 {
		if _, err := store.Delete(r.Context(), successIDs); err != nil {
			deleteErr = "failed to delete redriven entries"
			s.emitAudit(r, "dlq.redrive", "dlq", "", "partial_failure", map[string]any{
				"redriven": len(successIDs),
				"error":    deleteErr + ": " + err.Error(),
			})
		}
	}

	if deleteErr == "" {
		s.emitAudit(r, "dlq.redrive", "dlq", "", "success", map[string]any{
			"redriven": len(successIDs),
			"failed":   len(redriveErrors),
			"ids":      body.IDs,
		})
	}

	resp := map[string]any{
		"redriven": len(successIDs),
		"failed":   len(redriveErrors),
	}
	if len(redriveErrors) > 0 {
		resp["errors"] = redriveErrors
	}
	if deleteErr != "" {
		resp["warning"] = deleteErr
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDLQDeleteByIDs(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	store := rt.DLQStore()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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
	store := rt.DLQStore()
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	filter := domain.DLQFilter{
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
	store := rt.DLQStore()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	count, err := store.Purge(r.Context(), s.clk.Now().UTC())
	if err != nil {
		s.emitAudit(r, "dlq.purge", "dlq", "", "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusInternalServerError, "DLQ purge failed")
		return
	}
	s.emitAudit(r, "dlq.purge", "dlq", "", "success", map[string]any{"purged": count})
	writeJSON(w, http.StatusOK, map[string]int{"purged": count})
}
