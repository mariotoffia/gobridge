package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/mariotoffia/gobridge/ports"
)

// handleConfigCreate accepts a complete logical document, never an overlay on
// a synthetic running configuration. Existing documents always win.
func (s *Server) handleConfigCreate(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ConfigReadOnly {
		writeErr(w, http.StatusForbidden, "configuration repository is read-only")
		return
	}
	initializer, ok := s.cfg.ConfigStore.(ports.ConfigInitializer)
	if !ok || s.cfg.ConfigDecoder == nil {
		writeErr(w, http.StatusNotImplemented, "initial configuration creation is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid configuration document")
		return
	}
	cfg, err := s.cfg.ConfigDecoder(bytes.NewReader(body))
	if err != nil || cfg == nil {
		writeErr(w, http.StatusBadRequest, "invalid configuration document")
		return
	}
	// Admission gets its own decoded object. Mutations, including resolved
	// credentials, can never enter the logical document we publish.
	admission, err := s.cfg.ConfigDecoder(bytes.NewReader(body))
	if err == nil && admission != nil {
		_, err = s.cfg.ConfigStore.Validate(r.Context(), admission)
		if err == nil && s.cfg.ConfigAdmitter != nil {
			err = s.cfg.ConfigAdmitter(r.Context(), admission)
		}
	}
	if admission == nil {
		writeErr(w, http.StatusBadRequest, "invalid configuration document")
		return
	}
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "configuration admission failed")
		return
	}
	created, createErr := initializer.CreateIfAbsent(r.Context(), cfg)
	// Even an error may follow a committed write. Always reconcile; never undo
	// a winning document, and never apply the request's in-memory candidate.
	current, readErr := s.cfg.ConfigStore.Load(r.Context())
	if readErr != nil || current == nil {
		writeErr(w, http.StatusServiceUnavailable, "configuration creation outcome unavailable; inspect repository")
		return
	}
	if createErr != nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "commit_outcome_unknown", "version": current.Version})
		return
	}
	if !created {
		writeJSON(w, http.StatusConflict, map[string]any{"status": "configuration_exists", "version": current.Version})
		return
	}
	status, code := "committed", http.StatusCreated
	if s.cfg.ConfigApplier != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), commitApplyTimeout)
		defer cancel()
		if err := s.cfg.ConfigApplier(ctx, current); err != nil {
			status, code = "committed_not_applied", http.StatusAccepted
		}
	}
	s.emitAudit(r, "config.create", "config", "", status, nil)
	writeJSON(w, code, map[string]any{"status": status, "version": current.Version})
}
