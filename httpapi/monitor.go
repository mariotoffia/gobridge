package httpapi

import (
	"net/http"
)

// MonitorMux returns a ServeMux wired with monitor routes. It is intended
// for tests and ad-hoc mounting; production servers use Start, which applies
// middleware around the same route registration.
func (s *Server) MonitorMux() *http.ServeMux {
	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	return mux
}

func (s *Server) registerMonitorRoutes(mux *http.ServeMux) {
	const prefix = "/api/v1/monitor"

	// Unauthenticated probes for load balancers and orchestrators.
	mux.HandleFunc("GET "+prefix+"/health", s.handleHealth)
	mux.HandleFunc("GET "+prefix+"/live", s.handleLive)
	mux.HandleFunc("GET "+prefix+"/ready", s.handleReady)

	// Sensitive endpoints require authentication.
	mux.HandleFunc("GET "+prefix+"/topology", s.requireMonitorAuth(s.handleTopology))
	mux.HandleFunc("GET "+prefix+"/routes", s.requireMonitorAuth(s.handleMonitorRoutes))
	mux.HandleFunc("GET "+prefix+"/deephealth", s.requireMonitorAuth(s.handleDeepHealth))
}

// --- Unauthenticated probes ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, max-age=0")
	status := "ok"
	httpStatus := http.StatusOK
	if !s.rt.Healthy() {
		status = "unhealthy"
		httpStatus = http.StatusServiceUnavailable
	} else if !s.rt.IsRunning() {
		status = "not_running"
		httpStatus = http.StatusServiceUnavailable
	}
	resp := map[string]any{
		"status":      status,
		"instance_id": s.rt.InstanceID(),
		"routes":      len(s.rt.Routes()),
	}
	if compErrs := s.rt.ComponentErrors(); len(compErrs) > 0 {
		resp["failed_components"] = len(compErrs)
	}
	writeJSON(w, httpStatus, resp)
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, max-age=0")
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, max-age=0")
	if !s.rt.IsRunning() || !s.rt.Healthy() {
		writeErr(w, http.StatusServiceUnavailable, "not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready",
		"role":   s.rt.Role(),
	})
}

// --- Authenticated sensitive endpoints ---

// topologyRouteView is a compact route projection for topology responses.
type topologyRouteView struct {
	ID           string `json:"id"`
	DeliveryMode string `json:"delivery_mode"`
	DispatchMode string `json:"dispatch_mode"`
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	routes := s.rt.Routes()
	views := make([]topologyRouteView, len(routes))
	for i, ri := range routes {
		views[i] = topologyRouteView{
			ID:           ri.ID,
			DeliveryMode: string(ri.DeliveryMode),
			DispatchMode: string(ri.DispatchMode),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instance_id": s.rt.InstanceID(),
		"running":     s.rt.IsRunning(),
		"routes":      views,
	})
}

// monitorRouteView is a detailed route view with policy fields.
type monitorRouteView struct {
	ID              string `json:"id"`
	DeliveryMode    string `json:"delivery_mode"`
	DispatchMode    string `json:"dispatch_mode"`
	MaxInFlight     int    `json:"max_in_flight"`
	MaxReplay       int    `json:"max_replay"`
	AckAfter        string `json:"ack_after"`
	OnExpired       string `json:"on_expired"`
	OnPermFailure   string `json:"on_perm_failure"`
}

func (s *Server) handleMonitorRoutes(w http.ResponseWriter, r *http.Request) {
	routes := s.rt.Routes()
	views := make([]monitorRouteView, len(routes))
	for i, ri := range routes {
		views[i] = monitorRouteView{
			ID:            ri.ID,
			DeliveryMode:  string(ri.DeliveryMode),
			DispatchMode:  string(ri.DispatchMode),
			MaxInFlight:   ri.Policy.MaxInFlight,
			MaxReplay:     ri.Policy.MaxReplayAttempts,
			AckAfter:      string(ri.Policy.AckAfter),
			OnExpired:     string(ri.Policy.OnExpired),
			OnPermFailure: string(ri.Policy.OnPermanentFailure),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": views})
}

// deepHealthResponse is the JSON-serializable representation of a deep
// health check. It mirrors runtime.DeepHealth with explicit JSON tags.
type deepHealthResponse struct {
	Running         bool                        `json:"running"`
	Healthy         bool                        `json:"healthy"`
	InstanceID      string                      `json:"instance_id"`
	Role            string                      `json:"role"`
	ReadyForTraffic bool                        `json:"ready_for_traffic"`
	ServiceLevel    string                      `json:"service_level"`
	Sessions        []deepHealthSessionResponse `json:"sessions"`
	Routes          []deepHealthRouteResponse   `json:"routes"`
}

type deepHealthSessionResponse struct {
	SessionID           string `json:"session_id"`
	Connected           bool   `json:"connected"`
	HasLease            bool   `json:"has_lease"`
	SubscriptionsWanted int    `json:"subscriptions_wanted"`
	SubscriptionsActive int    `json:"subscriptions_active"`
	Ready               bool   `json:"ready"`
	ServiceLevel        string `json:"service_level"`
}

type deepHealthRouteResponse struct {
	ID           string `json:"id"`
	DeliveryMode string `json:"delivery_mode"`
}

func (s *Server) handleDeepHealth(w http.ResponseWriter, r *http.Request) {
	dh := s.rt.DeepHealth(r.Context())

	resp := deepHealthResponse{
		Running:         dh.Running,
		Healthy:         dh.Healthy,
		InstanceID:      dh.InstanceID,
		Role:            dh.Role,
		ReadyForTraffic: dh.ReadyForTraffic,
		ServiceLevel:    string(dh.ServiceLevel),
	}

	resp.Sessions = make([]deepHealthSessionResponse, len(dh.Sessions))
	for i, sh := range dh.Sessions {
		resp.Sessions[i] = deepHealthSessionResponse{
			SessionID:           sh.SessionID,
			Connected:           sh.Connected,
			HasLease:            sh.HasLease,
			SubscriptionsWanted: sh.SubscriptionsWanted,
			SubscriptionsActive: sh.SubscriptionsActive,
			Ready:               sh.Ready,
			ServiceLevel:        string(sh.ServiceLevel),
		}
	}

	resp.Routes = make([]deepHealthRouteResponse, len(dh.Routes))
	for i, rh := range dh.Routes {
		resp.Routes[i] = deepHealthRouteResponse{
			ID:           rh.ID,
			DeliveryMode: rh.DeliveryMode,
		}
	}

	status := http.StatusOK
	if !dh.ReadyForTraffic {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}
