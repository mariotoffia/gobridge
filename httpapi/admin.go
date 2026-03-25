package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	const prefix = "/api/v1/admin"

	mux.HandleFunc(prefix+"/bridge", s.requireAuth(s.handleBridge))
	mux.HandleFunc(prefix+"/bridge/start", s.requireAuth(s.handleStart))
	mux.HandleFunc(prefix+"/bridge/stop", s.requireAuth(s.handleStop))

	mux.HandleFunc(prefix+"/routes", s.requireAuth(s.handleRoutes))

	mux.HandleFunc(prefix+"/dlq", s.requireAuth(s.handleDLQ))
	mux.HandleFunc(prefix+"/dlq/messages", s.requireAuth(s.handleDLQMessages))
	mux.HandleFunc(prefix+"/dlq/replay", s.requireAuth(s.handleDLQReplay))
	mux.HandleFunc(prefix+"/dlq/purge", s.requireAuth(s.handleDLQPurge))
}

func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
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
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
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
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
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
	if err := store.Replay(r.Context(), body.IDs); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
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
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"purged": count})
}
