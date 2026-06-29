package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
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
	mux.HandleFunc("GET "+prefix+"/dlq/messages/{id}", s.requireAdminAuth(s.handleDLQMessageByID))
	mux.HandleFunc("POST "+prefix+"/dlq/redrive", s.requireAdminAuth(s.handleDLQRedrive))
	mux.HandleFunc("POST "+prefix+"/dlq/delete", s.requireAdminAuth(s.handleDLQDeleteByIDs))
	mux.HandleFunc("POST "+prefix+"/dlq/delete-by-filter", s.requireAdminAuth(s.handleDLQDeleteByFilter))
	mux.HandleFunc("POST "+prefix+"/dlq/purge", s.requireAdminAuth(s.handleDLQPurge))

	s.registerConfigRoutes(mux)
}

func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	s.emitAudit(r, "bridge.status", "bridge", "", "success", nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"instance_id": rt.InstanceID(),
		"running":     rt.IsRunning(),
		"routes":      len(rt.Routes()),
	})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.AdminOperationTimeout)
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		s.emitAudit(r, "bridge.start", "bridge", "", "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusConflict, "bridge start failed")
		return
	}
	s.emitAudit(r, "bridge.start", "bridge", "", "success", nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.AdminOperationTimeout)
	defer cancel()
	if err := rt.Stop(ctx); err != nil {
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
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	routes := rt.Routes()
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

type injectRequest struct {
	// ID, when present (even as the empty string), is treated as a
	// caller-supplied envelope ID. A nil pointer (field omitted)
	// triggers server-side generation via Server.idGen; an explicit
	// empty string is a 400 invalid_payload because it is almost
	// always a client bug — this is the same contract NewEnvelope
	// enforces and the admin endpoint must not paper over it.
	ID      *string        `json:"id,omitempty"`
	Subject string         `json:"subject"`
	Payload string         `json:"payload"` // base64-encoded
	Headers map[string]any `json:"headers"`
}

// defaultIDGen produces a 32-character hex envelope ID from
// crypto/rand. The format matches the AMQP/SQS adapter ID fallbacks
// (see amqp091.generateEnvelopeID) so envelope IDs are uniformly
// shaped across origin paths. crypto/rand failure is fatal because
// the platform RNG being unavailable is unrecoverable.
func defaultIDGen() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("httpapi: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
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

	envelopeID := ""
	switch {
	case body.ID == nil:
		envelopeID = s.idGen()
	case *body.ID == "":
		s.emitAudit(r, "route.inject", "route", routeID, "failure", map[string]any{
			"error": string(shared.ErrCodeInvalidPayload),
		})
		writeErr(w, http.StatusBadRequest, "envelope id must not be empty")
		return
	default:
		envelopeID = *body.ID
	}

	env, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:      envelopeID,
		Subject: body.Subject,
		Payload: payload,
		Headers: body.Headers,
	}, s.clk.Now())
	if err != nil {
		s.emitAudit(r, "route.inject", "route", routeID, "failure", map[string]any{
			"envelope_id": envelopeID,
			"error":       string(shared.ErrCodeInvalidPayload),
		})
		writeErr(w, http.StatusBadRequest, "invalid envelope: "+err.Error())
		return
	}

	if err := rt.Inject(r.Context(), routeID, env); err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			s.emitAudit(r, "route.inject", "route", routeID, "failure", map[string]any{
				"envelope_id": env.ID(),
				"error":       "route not found",
			})
			writeErr(w, http.StatusNotFound, "route not found")
			return
		}
		s.emitAudit(r, "route.inject", "route", routeID, "failure", map[string]any{
			"envelope_id": env.ID(),
			"error":       err.Error(),
		})
		writeErr(w, http.StatusInternalServerError, "message injection failed")
		return
	}

	s.emitAudit(r, "route.inject", "route", routeID, "success", map[string]any{
		"envelope_id": env.ID(),
		"subject":     body.Subject,
	})
	writeJSON(w, http.StatusOK, map[string]string{
		"status":      "injected",
		"envelope_id": env.ID(),
	})
}

func (s *Server) emitAudit(r *http.Request, action, resource, resourceID, outcome string, detail map[string]any) {
	s.audit.Log(r.Context(), ports.AuditEvent{
		Timestamp:  s.clk.Now().UTC(),
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
