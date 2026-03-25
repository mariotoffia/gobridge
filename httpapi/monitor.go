package httpapi

import (
	"net/http"
)

func (s *Server) registerMonitorRoutes(mux *http.ServeMux) {
	const prefix = "/api/v1/monitor"

	mux.HandleFunc(prefix+"/health", s.handleHealth)
	mux.HandleFunc(prefix+"/live", s.handleLive)
	mux.HandleFunc(prefix+"/ready", s.handleReady)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
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
		errMap := make(map[string]string, len(compErrs))
		for k, v := range compErrs {
			errMap[k] = v.Error()
		}
		resp["component_errors"] = errMap
	}
	writeJSON(w, httpStatus, resp)
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.rt.IsRunning() || !s.rt.Healthy() {
		writeErr(w, http.StatusServiceUnavailable, "not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
