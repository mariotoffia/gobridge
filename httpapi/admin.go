package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	const prefix = "/api/v1/admin"

	mux.HandleFunc(prefix+"/bridge", s.requireAdminAuth(s.handleBridge))
	mux.HandleFunc(prefix+"/bridge/start", s.requireAdminAuth(s.handleStart))
	mux.HandleFunc(prefix+"/bridge/stop", s.requireAdminAuth(s.handleStop))

	mux.HandleFunc(prefix+"/routes", s.requireAdminAuth(s.handleRoutes))
	mux.HandleFunc("POST "+prefix+"/routes/{routeID}/inject", s.requireAdminAuth(s.handleInject))

	mux.HandleFunc(prefix+"/dlq", s.requireAdminAuth(s.handleDLQ))
	mux.HandleFunc(prefix+"/dlq/messages", s.requireAdminAuth(s.handleDLQMessages))
	mux.HandleFunc(prefix+"/dlq/replay", s.requireAdminAuth(s.handleDLQReplay))
	mux.HandleFunc(prefix+"/dlq/purge", s.requireAdminAuth(s.handleDLQPurge))
}

func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.emitAudit(r, "bridge.status", "bridge", "", "success", nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"instance_id": s.rt.InstanceID(),
		"running":     s.rt.IsRunning(),
		"routes":      len(s.rt.Routes()),
	})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.rt.Start(ctx); err != nil {
		s.emitAudit(r, "bridge.start", "bridge", "", "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	s.emitAudit(r, "bridge.start", "bridge", "", "success", nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.rt.Stop(ctx); err != nil {
		s.emitAudit(r, "bridge.stop", "bridge", "", "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusInternalServerError, err.Error())
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
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
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
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleDLQ(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	store := s.rt.DLQStore()
	if store == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no DLQ store configured"})
		return
	}
	entries, err := store.List(r.Context(), domain.DLQFilter{Limit: 100})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":   len(entries),
		"entries": entries,
	})
}

func (s *Server) handleDLQMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	store := s.rt.DLQStore()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}
	filter := domain.DLQFilter{
		RouteID:  r.URL.Query().Get("route_id"),
		Category: r.URL.Query().Get("category"),
		Limit:    100,
	}
	entries, err := store.List(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleDLQReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
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
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.emitAudit(r, "dlq.replay", "dlq", "", "success", map[string]any{
		"count": len(body.IDs),
		"ids":   body.IDs,
	})
	writeJSON(w, http.StatusOK, map[string]int{"replayed": len(body.IDs)})
}

func (s *Server) handleDLQPurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	store := s.rt.DLQStore()
	if store == nil {
		writeErr(w, http.StatusNotFound, "no DLQ store configured")
		return
	}
	count, err := store.Purge(r.Context(), time.Now())
	if err != nil {
		s.emitAudit(r, "dlq.purge", "dlq", "", "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusInternalServerError, err.Error())
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
		Headers: body.Headers,
	}

	if err := s.rt.Inject(r.Context(), routeID, env); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.emitAudit(r, "route.inject", "route", routeID, "failure", map[string]any{
				"error": "route not found",
			})
			writeErr(w, http.StatusNotFound, "route not found: "+routeID)
			return
		}
		s.emitAudit(r, "route.inject", "route", routeID, "failure", map[string]any{
			"error": err.Error(),
		})
		writeErr(w, http.StatusInternalServerError, err.Error())
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
