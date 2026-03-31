package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	const prefix = "/api/v1/admin"

	mux.HandleFunc("GET "+prefix+"/bridge", s.requireAdminAuth(s.handleBridge))
	mux.HandleFunc("POST "+prefix+"/bridge/start", s.requireAdminAuth(s.handleStart))
	mux.HandleFunc("POST "+prefix+"/bridge/stop", s.requireAdminAuth(s.handleStop))

	mux.HandleFunc("GET "+prefix+"/routes", s.requireAdminAuth(s.handleRoutes))
	mux.HandleFunc("POST "+prefix+"/routes/{routeID}/inject", s.requireAdminAuth(s.handleInject))

	mux.HandleFunc("GET "+prefix+"/dlq", s.requireAdminAuth(s.handleDLQ))
	mux.HandleFunc("GET "+prefix+"/dlq/messages", s.requireAdminAuth(s.handleDLQMessages))
	mux.HandleFunc("POST "+prefix+"/dlq/replay", s.requireAdminAuth(s.handleDLQReplay))
	mux.HandleFunc("POST "+prefix+"/dlq/purge", s.requireAdminAuth(s.handleDLQPurge))

	s.registerConfigRoutes(mux)
}

func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	s.emitAudit(r, "bridge.status", "bridge", "", "success", nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"instance_id": s.rt.InstanceID(),
		"running":     s.rt.IsRunning(),
		"routes":      len(s.rt.Routes()),
	})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.rt.Start(ctx); err != nil {
		s.emitAudit(r, "bridge.start", "bridge", "", "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusConflict, "bridge start failed")
		return
	}
	s.emitAudit(r, "bridge.start", "bridge", "", "success", nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.rt.Stop(ctx); err != nil {
		s.emitAudit(r, "bridge.stop", "bridge", "", "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusInternalServerError, "bridge stop failed")
		return
	}
	s.emitAudit(r, "bridge.stop", "bridge", "", "success", nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

type routeView struct {
	ID           string `json:"id"`
	DeliveryMode string `json:"delivery_mode"`
	DispatchMode string `json:"dispatch_mode"`
	MaxInFlight  int    `json:"max_in_flight"`
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	routes := s.rt.Routes()
	views := make([]routeView, len(routes))
	for i, ri := range routes {
		views[i] = routeView{
			ID:           ri.ID,
			DeliveryMode: string(ri.DeliveryMode),
			DispatchMode: string(ri.DispatchMode),
			MaxInFlight:  ri.Policy.MaxInFlight,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": views})
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

func (s *Server) handleDLQ(w http.ResponseWriter, r *http.Request) {
	store := s.rt.DLQStore()
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

const (
	defaultDLQLimit = 100
	maxDLQLimit     = 1000
)

func (s *Server) handleDLQMessages(w http.ResponseWriter, r *http.Request) {
	store := s.rt.DLQStore()
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
		"total":    len(entries),
		"limit":    limit,
		"offset":   offset,
	})
}

func (s *Server) handleDLQReplay(w http.ResponseWriter, r *http.Request) {
	store := s.rt.DLQStore()
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
	const maxReplayIDs = 1000
	if len(body.IDs) > maxReplayIDs {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("ids exceeds maximum of %d", maxReplayIDs))
		return
	}
	if err := store.Replay(r.Context(), body.IDs); err != nil {
		s.emitAudit(r, "dlq.replay", "dlq", "", "failure", map[string]any{
			"ids":   body.IDs,
			"error": err.Error(),
		})
		writeErr(w, http.StatusInternalServerError, "DLQ replay failed")
		return
	}
	s.emitAudit(r, "dlq.replay", "dlq", "", "success", map[string]any{
		"count": len(body.IDs),
		"ids":   body.IDs,
	})
	writeJSON(w, http.StatusOK, map[string]int{"replayed": len(body.IDs)})
}

func (s *Server) handleDLQPurge(w http.ResponseWriter, r *http.Request) {
	store := s.rt.DLQStore()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	count, err := store.Purge(r.Context(), time.Now().UTC())
	if err != nil {
		s.emitAudit(r, "dlq.purge", "dlq", "", "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusInternalServerError, "DLQ purge failed")
		return
	}
	s.emitAudit(r, "dlq.purge", "dlq", "", "success", map[string]any{"purged": count})
	writeJSON(w, http.StatusOK, map[string]int{"purged": count})
}

type injectRequest struct {
	Subject string         `json:"subject"`
	Payload string         `json:"payload"` // base64-encoded
	Headers map[string]any `json:"headers"`
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	routeID := r.PathValue("routeID")

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body injectRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var payload []byte
	if body.Payload != "" {
		var err error
		payload, err = base64.StdEncoding.DecodeString(body.Payload)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "payload must be base64-encoded")
			return
		}
	}

	env := &domain.Envelope{
		Subject: body.Subject,
		Payload: payload,
		Headers: domain.StripReservedHeaders(body.Headers),
	}

	if err := s.rt.Inject(r.Context(), routeID, env); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.emitAudit(r, "route.inject", "route", routeID, "failure", map[string]any{
				"error": "route not found",
			})
			writeErr(w, http.StatusNotFound, "route not found")
			return
		}
		s.emitAudit(r, "route.inject", "route", routeID, "failure", map[string]any{
			"error": err.Error(),
		})
		writeErr(w, http.StatusInternalServerError, "message injection failed")
		return
	}

	s.emitAudit(r, "route.inject", "route", routeID, "success", map[string]any{
		"subject": body.Subject,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "injected"})
}

func (s *Server) emitAudit(r *http.Request, action, resource, resourceID, outcome string, detail map[string]any) {
	s.audit.Log(r.Context(), ports.AuditEvent{
		Timestamp:  time.Now().UTC(),
		Action:     action,
		Actor:      actorFromRequest(r),
		Resource:   resource,
		ResourceID: resourceID,
		Outcome:    outcome,
		Detail:     detail,
	})
}

func actorFromRequest(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	return r.RemoteAddr
}
