package httpapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// registerConfigRoutes adds config management endpoints to the admin mux.
// It is a no-op when the config transaction manager is not configured.
func (s *Server) registerConfigRoutes(mux *http.ServeMux) {
	if s.configTxn == nil {
		return
	}

	const prefix = "/api/v1/admin/config"

	mux.HandleFunc("GET "+prefix, s.requireAdminAuth(s.handleConfigGet))
	mux.HandleFunc("POST "+prefix+"/transactions", s.requireAdminAuth(s.handleConfigTxnCreate))
	mux.HandleFunc("GET "+prefix+"/transactions/{txnID}", s.requireAdminAuth(s.handleConfigTxnGet))
	mux.HandleFunc("PATCH "+prefix+"/transactions/{txnID}", s.requireAdminAuth(s.handleConfigTxnPatch))
	mux.HandleFunc("POST "+prefix+"/transactions/{txnID}/commit", s.requireAdminAuth(s.handleConfigTxnCommit))
	mux.HandleFunc("DELETE "+prefix+"/transactions/{txnID}", s.requireAdminAuth(s.handleConfigTxnRollback))
}

// handleConfigGet returns the current effective config with sensitive
// fields redacted.
func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.configTxn.configProvider()
	if cfg == nil {
		writeErr(w, http.StatusServiceUnavailable, "no config available")
		return
	}

	s.emitAudit(r, "config.read", "config", "", "success", nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"config": sanitizeConfig(cfg),
	})
}

// configTxnCreateRequest is the optional body for POST /transactions.
type configTxnCreateRequest struct {
	TTL string `json:"ttl"`
}

// handleConfigTxnCreate starts a new config transaction.
func (s *Server) handleConfigTxnCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// An empty body legitimately means "use defaults": json.Decode
	// returns io.EOF for an empty or whitespace-only body, which we
	// treat as the zero-valued request. A non-empty but malformed body
	// must be rejected rather than silently falling back to a default
	// transaction.
	var ttl time.Duration
	var body configTxnCreateRequest
	if err := decodeStrictJSON(r.Body, &body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.TTL != "" {
		var parseErr error
		ttl, parseErr = time.ParseDuration(body.TTL)
		if parseErr != nil {
			writeErr(w, http.StatusBadRequest, "invalid ttl duration")
			return
		}
	}

	txn, err := s.configTxn.Begin(ttl)
	if err != nil {
		if errors.Is(err, errTxnActive) {
			s.emitAudit(r, "config.txn.create", "config", "", "failure", map[string]any{
				"error": "transaction already active",
			})
			writeErr(w, http.StatusConflict, "another config transaction is already active")
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to create transaction")
		return
	}

	cfg := s.configTxn.configProvider()

	s.emitAudit(r, "config.txn.create", "config", txn.ID, "success", nil)
	writeJSON(w, http.StatusCreated, map[string]any{
		"txn_id":     txn.ID,
		"created_at": txn.CreatedAt.Format(time.RFC3339),
		"expires_at": txn.ExpiresAt.Format(time.RFC3339),
		"config":     sanitizeConfig(cfg),
	})
}

// handleConfigTxnGet returns the state of the active transaction and
// a preview of the merged config.
func (s *Server) handleConfigTxnGet(w http.ResponseWriter, r *http.Request) {
	txnID := r.PathValue("txnID")

	preview, err := s.configTxn.Preview(r.Context(), txnID)
	if err != nil {
		s.writeConfigTxnError(w, r, "config.txn.read", txnID, err)
		return
	}

	txn := s.configTxn.Active()
	s.emitAudit(r, "config.txn.read", "config", txnID, "success", nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"txn_id":      txn.ID,
		"created_at":  txn.CreatedAt.Format(time.RFC3339),
		"expires_at":  txn.ExpiresAt.Format(time.RFC3339),
		"patch_count": txn.PatchCount,
		"preview":     sanitizeConfig(preview),
	})
}

// handleConfigTxnPatch applies a config overlay to the active transaction.
func (s *Server) handleConfigTxnPatch(w http.ResponseWriter, r *http.Request) {
	txnID := r.PathValue("txnID")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var overlay ports.BridgeConfig
	if err := decodeStrictJSON(r.Body, &overlay); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	merged, warnings, err := s.configTxn.Patch(r.Context(), txnID, &overlay)
	if err != nil {
		s.writeConfigTxnError(w, r, "config.txn.patch", txnID, err)
		return
	}

	s.emitAudit(r, "config.txn.patch", "config", txnID, "success", nil)
	resp := map[string]any{
		"txn_id":  txnID,
		"preview": sanitizeConfig(merged),
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleConfigTxnCommit validates and writes the config to disk.
func (s *Server) handleConfigTxnCommit(w http.ResponseWriter, r *http.Request) {
	txnID := r.PathValue("txnID")

	newVersion, err := s.configTxn.Commit(r.Context(), txnID)
	if err != nil {
		s.writeConfigTxnError(w, r, "config.txn.commit", txnID, err)
		return
	}

	s.emitAudit(r, "config.txn.commit", "config", txnID, "success", map[string]any{
		"version": newVersion,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "committed",
		"version": newVersion,
	})
}

// handleConfigTxnRollback discards the active transaction.
func (s *Server) handleConfigTxnRollback(w http.ResponseWriter, r *http.Request) {
	txnID := r.PathValue("txnID")

	if err := s.configTxn.Rollback(txnID); err != nil {
		s.writeConfigTxnError(w, r, "config.txn.rollback", txnID, err)
		return
	}

	s.emitAudit(r, "config.txn.rollback", "config", txnID, "success", nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "rolled_back"})
}

// writeConfigTxnError maps transaction manager errors to HTTP responses.
func (s *Server) writeConfigTxnError(w http.ResponseWriter, r *http.Request, action, txnID string, err error) {
	switch {
	case errors.Is(err, errTxnNotFound), errors.Is(err, errTxnExpired):
		s.emitAudit(r, action, "config", txnID, "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusNotFound, "transaction not found or expired")

	case errors.Is(err, errVersionConflict):
		s.emitAudit(r, action, "config", txnID, "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusConflict, err.Error())

	case isValidationError(err):
		ve := extractValidationError(err)
		s.emitAudit(r, action, "config", txnID, "failure", map[string]any{
			"error":             "validation failed",
			"validation_errors": ve.Errors,
		})
		resp := map[string]any{
			"error":             "config validation failed",
			"validation_errors": ve.Errors,
		}
		if len(ve.Warnings) > 0 {
			resp["warnings"] = ve.Warnings
		}
		writeJSON(w, http.StatusUnprocessableEntity, resp)

	default:
		s.emitAudit(r, action, "config", txnID, "failure", map[string]any{"error": err.Error()})
		writeErr(w, http.StatusInternalServerError, "config operation failed")
	}
}

// sanitizeConfig returns a shallow copy of the config for JSON
// responses with secret values EXPLICITLY redacted at this read
// boundary. shared.Secret now also redacts structurally on marshal
// (json.Marshal yields "[REDACTED]"), so this explicit redaction is
// defense in depth — it keeps the read path correct even if the
// response shape ever changes to surface a Secret through a different
// marshaller. The returned value must NOT be mutated — non-secret
// nested fields (Sessions, Routes, Stores, etc.) still share references
// with the original.
func sanitizeConfig(cfg *ports.BridgeConfig) *ports.BridgeConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	// Redact the HTTP admin/monitor API keys via a deep copy of the
	// HTTP block so the original config is left untouched.
	if cfg.HTTP != nil {
		httpCopy := *cfg.HTTP
		httpCopy.AdminAPIKey = redactSecret(httpCopy.AdminAPIKey)
		httpCopy.MonitorAPIKey = redactSecret(httpCopy.MonitorAPIKey)
		out.HTTP = &httpCopy
	}
	// Receiver/Sender plugin configs live in the typed Config field
	// (json:"-"), so secrets in adapter-specific config never reach
	// the JSON response and need no extra redaction here.
	if len(cfg.Receivers) > 0 {
		out.Receivers = append([]ports.ReceiverDef(nil), cfg.Receivers...)
	}
	if len(cfg.Senders) > 0 {
		out.Senders = append([]ports.SenderDef(nil), cfg.Senders...)
	}
	return &out
}

// redactSecret returns a redaction-marker secret for a non-empty value
// and leaves an empty secret empty, so the admin config response
// signals "a key is configured" without exposing it.
func redactSecret(s shared.Secret) shared.Secret {
	if s.IsZero() {
		return s
	}
	return shared.RedactedSecret()
}

// isValidationError checks whether the error is a blueprint
// validation error reported by the configured ports.ConfigStore.
func isValidationError(err error) bool {
	var ve *ports.BlueprintValidationError
	return errors.As(err, &ve)
}

// extractValidationError unwraps a *ports.BlueprintValidationError
// from err. Returns a synthetic value carrying err.Error() when err
// is not a validation error.
func extractValidationError(err error) *ports.BlueprintValidationError {
	var ve *ports.BlueprintValidationError
	if errors.As(err, &ve) {
		return ve
	}
	return &ports.BlueprintValidationError{Errors: []string{err.Error()}}
}
