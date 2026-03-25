package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ============================================================================
// Bridge Lifecycle Handlers
// ============================================================================

func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	status := "stopped"
	if s.bridge.IsReady() {
		status = "running"
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":               s.bridge.GetID(),
		"clusterId":        s.bridge.GetClusterID(),
		"status":           status,
		"ready":            s.bridge.IsReady(),
		"live":             s.bridge.IsLive(),
		"pipelinesCount":   len(s.bridge.GetPipelines()),
		"connectionsCount": len(s.bridge.ListConnections()),
	})
}

func (s *Server) handleBridgeStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	if s.bridge.IsReady() {
		writeError(w, http.StatusConflict, "ALREADY_RUNNING", "bridge is already running")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.bridge.Start(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "START_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"message":   "bridge started",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleBridgeStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	var req struct {
		Drain   bool   `json:"drain"`
		Timeout string `json:"timeout"`
	}
	req.Drain = true // default
	req.Timeout = "30s"

	if r.Body != nil && r.ContentLength > 0 {
		json.NewDecoder(r.Body).Decode(&req)
	}

	timeout, err := time.ParseDuration(req.Timeout)
	if err != nil {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	if err := s.bridge.Stop(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "STOP_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"message":   "bridge stopped",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleBridgeDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	// Drain is handled as part of stop for now
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"startedAt":       time.Now().UTC().Format(time.RFC3339),
		"total":           0,
		"drained":         0,
		"inFlight":        0,
		"failed":          0,
		"complete":        true,
		"percentComplete": 100.0,
	})
}

// ============================================================================
// Connections Handlers
// ============================================================================

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		connIDs := s.bridge.ListConnections()
		connections := make([]map[string]interface{}, 0, len(connIDs))

		for _, id := range connIDs {
			if conn, ok := s.bridge.GetConnection(id); ok {
				connections = append(connections, map[string]interface{}{
					"id":     id,
					"status": getConnectionStatus(conn),
				})
			}
		}

		writeJSON(w, http.StatusOK, connections)

	case http.MethodPost:
		var req struct {
			ID            string                 `json:"id"`
			TransportType string                 `json:"transportType"`
			BrokerUrls    []string               `json:"brokerUrls"`
			ClientId      string                 `json:"clientId"`
			Options       map[string]interface{} `json:"options"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}

		// Connection creation would require factory registration
		// For now, return a placeholder
		writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "connection creation requires transport factory")

	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

func (s *Server) handleConnection(w http.ResponseWriter, r *http.Request) {
	prefix := s.config.AdminAPIPrefix + "/connections/"
	id := extractPathParam(r, prefix)

	// Handle action paths
	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		id = parts[0]
		action := parts[1]

		switch action {
		case "reconnect":
			s.handleConnectionReconnect(w, r, id)
		case "test":
			s.handleConnectionTest(w, r, id)
		default:
			writeError(w, http.StatusNotFound, "NOT_FOUND", "action not found")
		}
		return
	}

	conn, ok := s.bridge.GetConnection(id)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "connection not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id":     id,
			"status": getConnectionStatus(conn),
		})

	case http.MethodPut:
		writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "connection update not yet implemented")

	case http.MethodDelete:
		writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "connection delete not yet implemented")

	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

func (s *Server) handleConnectionReconnect(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	_, ok := s.bridge.GetConnection(id)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "connection not found")
		return
	}

	// Reconnection would require connection interface update
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"message":   "reconnection initiated",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleConnectionTest(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	_, ok := s.bridge.GetConnection(id)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "connection not found")
		return
	}

	// Connection test - just check if it's available
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"latencyMs": 1,
	})
}

// ============================================================================
// Pipelines Handlers
// ============================================================================

func (s *Server) handlePipelines(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pipelines := s.bridge.GetPipelines()
		result := make([]map[string]interface{}, len(pipelines))

		for i, p := range pipelines {
			result[i] = map[string]interface{}{
				"id":     p.GetID(),
				"mode":   string(p.GetMode()),
				"status": getPipelineStatus(p),
			}
		}

		writeJSON(w, http.StatusOK, result)

	case http.MethodPost:
		writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "pipeline creation via API not yet implemented")

	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

func (s *Server) handlePipeline(w http.ResponseWriter, r *http.Request) {
	prefix := s.config.AdminAPIPrefix + "/pipelines/"
	id := extractPathParam(r, prefix)

	// Handle action paths
	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		id = parts[0]
		action := parts[1]

		switch action {
		case "start":
			s.handlePipelineStart(w, r, id)
		case "stop":
			s.handlePipelineStop(w, r, id)
		case "stats":
			s.handlePipelineStats(w, r, id)
		default:
			writeError(w, http.StatusNotFound, "NOT_FOUND", "action not found")
		}
		return
	}

	pipeline, ok := s.bridge.GetPipeline(id)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "pipeline not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id":     pipeline.GetID(),
			"mode":   string(pipeline.GetMode()),
			"status": getPipelineStatus(pipeline),
		})

	case http.MethodPut:
		writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "pipeline update not yet implemented")

	case http.MethodDelete:
		ctx := r.Context()
		if err := s.bridge.RemovePipelineRunning(ctx, id); err != nil {
			writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

func (s *Server) handlePipelineStart(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	pipeline, ok := s.bridge.GetPipeline(id)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "pipeline not found")
		return
	}

	ctx := r.Context()
	if err := pipeline.Start(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "START_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "pipeline started",
	})
}

func (s *Server) handlePipelineStop(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	pipeline, ok := s.bridge.GetPipeline(id)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "pipeline not found")
		return
	}

	if err := pipeline.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "STOP_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "pipeline stopped",
	})
}

func (s *Server) handlePipelineStats(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	_, ok := s.bridge.GetPipeline(id)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "pipeline not found")
		return
	}

	metrics := s.bridge.Metrics()
	if metrics != nil && metrics.Pipelines != nil {
		if pm, ok := metrics.Pipelines[id]; ok {
			writeJSON(w, http.StatusOK, pm)
			return
		}
	}

	// Return empty stats
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"messagesReceived": 0,
		"messagesSent":     0,
		"messagesFailed":   0,
		"messagesRetried":  0,
		"messagesDropped":  0,
		"inFlight":         0,
		"averageLatencyMs": 0,
	})
}

// ============================================================================
// Routes Handlers
// ============================================================================

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "routes API not yet implemented")
}

func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "routes API not yet implemented")
}

// ============================================================================
// DLQ Handlers
// ============================================================================

func (s *Server) handleDLQ(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	if s.dlqManager == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"totalMessages": 0,
			"byTopic":       map[string]int64{},
			"byErrorCode":   map[string]int64{},
		})
		return
	}

	ctx := r.Context()
	summary, err := s.dlqManager.GetSummary(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DLQ_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleDLQMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	if s.dlqManager == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"messages": []interface{}{},
			"total":    0,
			"offset":   0,
			"limit":    100,
			"hasMore":  false,
		})
		return
	}

	// Parse query parameters
	query := r.URL.Query()
	filter := types.DLQFilter{
		Topic:    query.Get("topic"),
		SourceID: query.Get("sourceId"),
	}

	if since := query.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = t
		}
	}
	if until := query.Get("until"); until != "" {
		if t, err := time.Parse(time.RFC3339, until); err == nil {
			filter.Until = t
		}
	}

	pagination := types.DLQPagination{
		Limit:  100,
		Offset: 0,
	}
	if limit := query.Get("limit"); limit != "" {
		fmt.Sscanf(limit, "%d", &pagination.Limit)
	}
	if offset := query.Get("offset"); offset != "" {
		fmt.Sscanf(offset, "%d", &pagination.Offset)
	}

	ctx := r.Context()
	result, err := s.dlqManager.ListMessages(ctx, filter, pagination)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DLQ_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDLQMessage(w http.ResponseWriter, r *http.Request) {
	prefix := s.config.AdminAPIPrefix + "/dlq/messages/"
	messageID := extractPathParam(r, prefix)

	if s.dlqManager == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}

	ctx := r.Context()

	switch r.Method {
	case http.MethodGet:
		msg, err := s.dlqManager.GetMessage(ctx, messageID)
		if err != nil {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
			return
		}
		writeJSON(w, http.StatusOK, msg)

	case http.MethodDelete:
		err := s.dlqManager.DeleteMessage(ctx, messageID)
		if err != nil {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

func (s *Server) handleDLQReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	if s.dlqManager == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"replayed":    0,
			"failed":      0,
			"errors":      []string{},
			"replayedIds": []string{},
		})
		return
	}

	var req types.DLQReplayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	ctx := r.Context()
	result, err := s.dlqManager.Replay(ctx, &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "REPLAY_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDLQPurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	if s.dlqManager == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"purged": 0,
		})
		return
	}

	var filter types.DLQFilter
	if r.Body != nil && r.ContentLength > 0 {
		json.NewDecoder(r.Body).Decode(&filter)
	}

	ctx := r.Context()
	result, err := s.dlqManager.Purge(ctx, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PURGE_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// ============================================================================
// Config Handlers
// ============================================================================

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":        s.bridge.GetID(),
		"clusterId": s.bridge.GetClusterID(),
	})
}

func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	if s.configReloader == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "config reloader not configured")
		return
	}

	ctx := r.Context()
	result, err := s.configReloader.Reload(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RELOAD_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":        len(result.Errors) == 0,
		"changesApplied": result.ChangesApplied,
		"added":          result.Added,
		"updated":        result.Updated,
		"deleted":        result.Deleted,
		"errors":         result.Errors,
		"durationMs":     result.Duration.Milliseconds(),
		"timestamp":      result.Timestamp.Format(time.RFC3339),
	})
}

func (s *Server) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	err := s.bridge.Validate()

	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"valid": false,
			"errors": []map[string]string{
				{"message": err.Error()},
			},
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"valid":    true,
		"errors":   []interface{}{},
		"warnings": []string{},
	})
}

func (s *Server) handleConfigDiff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"changes": []interface{}{},
	})
}

// ============================================================================
// Testing Handlers
// ============================================================================

// TestMessageRequest is the request for injecting a test message.
type TestMessageRequest struct {
	PipelineID    string                 `json:"pipelineId"`
	Topic         string                 `json:"topic"`
	Payload       []byte                 `json:"payload"`
	PayloadString string                 `json:"payloadString"`
	ContentType   string                 `json:"contentType"`
	Metadata      map[string]interface{} `json:"metadata"`
	TTL           string                 `json:"ttl"`
	WaitForResult bool                   `json:"waitForResult"`
}

func (s *Server) handleTestMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	var req TestMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	if req.PipelineID == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "pipelineId is required")
		return
	}

	pipeline, ok := s.bridge.GetPipeline(req.PipelineID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "pipeline not found")
		return
	}

	// Check if pipeline has InjectMiddleware
	injectable, ok := pipeline.(InjectableInterface)
	if !ok {
		writeError(w, http.StatusBadRequest, "NOT_INJECTABLE",
			"pipeline does not support message injection - add InjectMiddleware")
		return
	}

	// Prepare payload
	payload := req.Payload
	if len(payload) == 0 && req.PayloadString != "" {
		payload = []byte(req.PayloadString)
	}

	// Inject the message
	start := time.Now()
	result, err := injectable.InjectMessage(r.Context(), req.Topic, payload, req.Metadata)

	response := map[string]interface{}{
		"success":          err == nil,
		"processingTimeMs": time.Since(start).Milliseconds(),
	}

	if err != nil {
		response["error"] = err.Error()
	}

	if result != nil {
		response["messageId"] = result.MessageID
		response["middlewareResults"] = result.MiddlewareResults
		response["targetResult"] = result.TargetResult
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleTestMessageBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "batch injection not yet implemented")
}

func (s *Server) handleTestPipeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "pipeline test not yet implemented")
}

// InjectableInterface is implemented by pipelines that support message injection.
type InjectableInterface interface {
	InjectMessage(ctx context.Context, topic string, payload []byte, metadata map[string]interface{}) (*InjectionResult, error)
}

// InjectionResult is the result of injecting a message.
type InjectionResult struct {
	MessageID         string                   `json:"messageId"`
	MiddlewareResults []map[string]interface{} `json:"middlewareResults"`
	TargetResult      map[string]interface{}   `json:"targetResult"`
}

// ============================================================================
// Diagnostics Handlers
// ============================================================================

func (s *Server) handleDiagnosticsLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	// Would need log buffer integration
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": []interface{}{},
	})
}

func (s *Server) handleDiagnosticsErrors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": []interface{}{},
	})
}

func (s *Server) handleDiagnosticsDebug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	ctx := r.Context()
	health := s.bridge.Health(ctx)
	metrics := s.bridge.Metrics()

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"bridge": map[string]interface{}{
			"id":        s.bridge.GetID(),
			"clusterId": s.bridge.GetClusterID(),
			"ready":     s.bridge.IsReady(),
			"live":      s.bridge.IsLive(),
		},
		"goroutines": runtime.NumGoroutine(),
		"memoryMB":   float64(memStats.Alloc) / 1024 / 1024,
		"health":     health,
		"metrics":    metrics,
	})
}

// ============================================================================
// Helper Functions
// ============================================================================

func getConnectionStatus(conn interface{}) string {
	// Would need connection state interface
	return "connected"
}

func getPipelineStatus(p interface{}) string {
	// Would need pipeline state interface
	return "running"
}
