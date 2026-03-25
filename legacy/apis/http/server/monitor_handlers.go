package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ============================================================================
// Health Check Handlers
// ============================================================================

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	ctx := r.Context()
	health := s.bridge.Health(ctx)

	status := http.StatusOK
	if health != nil && health.Status != types.HealthStatusHealthy {
		if health.Status == types.HealthStatusUnhealthy {
			status = http.StatusServiceUnavailable
		}
	}

	writeJSON(w, status, health)
}

func (s *Server) handleHealthLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	live := s.bridge.IsLive()

	status := http.StatusOK
	if !live {
		status = http.StatusServiceUnavailable
	}

	writeJSON(w, status, map[string]interface{}{
		"live":    live,
		"message": liveMessage(live),
	})
}

func (s *Server) handleHealthReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	ready := s.bridge.IsReady()

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}

	checks := map[string]bool{
		"bridge": ready,
	}

	writeJSON(w, status, map[string]interface{}{
		"ready":   ready,
		"message": readyMessage(ready),
		"checks":  checks,
	})
}

func (s *Server) handleHealthStartup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	// Startup is considered complete if we're ready
	if s.bridge.IsReady() {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
}

// ============================================================================
// Metrics Handlers
// ============================================================================

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	metrics := s.bridge.Metrics()

	// Output Prometheus format
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	fmt.Fprintf(w, "# HELP gobridge_up Bridge is up and running\n")
	fmt.Fprintf(w, "# TYPE gobridge_up gauge\n")
	if s.bridge.IsReady() {
		fmt.Fprintf(w, "gobridge_up 1\n")
	} else {
		fmt.Fprintf(w, "gobridge_up 0\n")
	}
	fmt.Fprintln(w)

	if metrics == nil {
		return
	}

	// Pipeline metrics
	if metrics.Pipelines != nil {
		fmt.Fprintf(w, "# HELP gobridge_messages_received_total Total messages received\n")
		fmt.Fprintf(w, "# TYPE gobridge_messages_received_total counter\n")
		for id, pm := range metrics.Pipelines {
			fmt.Fprintf(w, "gobridge_messages_received_total{pipeline=%q} %d\n", id, pm.Stats.MessagesReceived)
		}
		fmt.Fprintln(w)

		fmt.Fprintf(w, "# HELP gobridge_messages_sent_total Total messages sent\n")
		fmt.Fprintf(w, "# TYPE gobridge_messages_sent_total counter\n")
		for id, pm := range metrics.Pipelines {
			fmt.Fprintf(w, "gobridge_messages_sent_total{pipeline=%q} %d\n", id, pm.Stats.MessagesSent)
		}
		fmt.Fprintln(w)

		fmt.Fprintf(w, "# HELP gobridge_messages_failed_total Total messages failed\n")
		fmt.Fprintf(w, "# TYPE gobridge_messages_failed_total counter\n")
		for id, pm := range metrics.Pipelines {
			fmt.Fprintf(w, "gobridge_messages_failed_total{pipeline=%q} %d\n", id, pm.Stats.MessagesFailed)
		}
		fmt.Fprintln(w)

		fmt.Fprintf(w, "# HELP gobridge_in_flight Current in-flight messages\n")
		fmt.Fprintf(w, "# TYPE gobridge_in_flight gauge\n")
		for id, pm := range metrics.Pipelines {
			fmt.Fprintf(w, "gobridge_in_flight{pipeline=%q} %d\n", id, pm.Stats.InFlight)
		}
		fmt.Fprintln(w)

		fmt.Fprintf(w, "# HELP gobridge_message_latency_seconds Message processing latency\n")
		fmt.Fprintf(w, "# TYPE gobridge_message_latency_seconds gauge\n")
		for id, pm := range metrics.Pipelines {
			fmt.Fprintf(w, "gobridge_message_latency_seconds{pipeline=%q} %f\n", id, pm.AverageLatency.Seconds())
		}
		fmt.Fprintln(w)
	}

	// Retry metrics
	fmt.Fprintf(w, "# HELP gobridge_retry_attempts_total Total retry attempts\n")
	fmt.Fprintf(w, "# TYPE gobridge_retry_attempts_total counter\n")
	fmt.Fprintf(w, "gobridge_retry_attempts_total{type=\"transport\"} %d\n", metrics.Retry.TransportRetryAttempts)
	fmt.Fprintf(w, "gobridge_retry_attempts_total{type=\"message\"} %d\n", metrics.Retry.MessageRetryAttempts)
	fmt.Fprintln(w)

	fmt.Fprintf(w, "# HELP gobridge_dlq_messages Total messages in DLQ\n")
	fmt.Fprintf(w, "# TYPE gobridge_dlq_messages gauge\n")
	fmt.Fprintf(w, "gobridge_dlq_messages %d\n", metrics.Retry.DLQMessages)
	fmt.Fprintln(w)

	// Flow control metrics
	fmt.Fprintf(w, "# HELP gobridge_backpressure_events_total Total backpressure events\n")
	fmt.Fprintf(w, "# TYPE gobridge_backpressure_events_total counter\n")
	fmt.Fprintf(w, "gobridge_backpressure_events_total %d\n", metrics.FlowControl.BackpressureEvents)
	fmt.Fprintln(w)
}

func (s *Server) handleMetricsJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	metrics := s.bridge.Metrics()
	writeJSON(w, http.StatusOK, metrics)
}

func (s *Server) handleMetricsPipelines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	metrics := s.bridge.Metrics()
	if metrics != nil && metrics.Pipelines != nil {
		result := make([]map[string]interface{}, 0, len(metrics.Pipelines))
		for id, pm := range metrics.Pipelines {
			result = append(result, map[string]interface{}{
				"id":               id,
				"messagesReceived": pm.Stats.MessagesReceived,
				"messagesSent":     pm.Stats.MessagesSent,
				"messagesFailed":   pm.Stats.MessagesFailed,
				"inFlight":         pm.Stats.InFlight,
				"averageLatencyMs": pm.AverageLatency.Milliseconds(),
			})
		}
		writeJSON(w, http.StatusOK, result)
		return
	}

	writeJSON(w, http.StatusOK, []interface{}{})
}

func (s *Server) handleMetricsPipeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	prefix := s.config.MonitorAPIPrefix + "/metrics/pipelines/"
	id := extractPathParam(r, prefix)

	metrics := s.bridge.Metrics()
	if metrics != nil && metrics.Pipelines != nil {
		if pm, ok := metrics.Pipelines[id]; ok {
			writeJSON(w, http.StatusOK, pm)
			return
		}
	}

	writeError(w, http.StatusNotFound, "NOT_FOUND", "pipeline not found")
}

func (s *Server) handleMetricsConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	metrics := s.bridge.Metrics()
	if metrics != nil && metrics.Connections != nil {
		result := make([]map[string]interface{}, 0, len(metrics.Connections))
		for id, cm := range metrics.Connections {
			result = append(result, map[string]interface{}{
				"id":             id,
				"status":         cm.Status,
				"reconnectCount": cm.ReconnectCount,
			})
		}
		writeJSON(w, http.StatusOK, result)
		return
	}

	writeJSON(w, http.StatusOK, []interface{}{})
}

func (s *Server) handleMetricsRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	metrics := s.bridge.Metrics()
	if metrics != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"transportRetry": map[string]interface{}{
				"attempts":  metrics.Retry.TransportRetryAttempts,
				"successes": metrics.Retry.TransportRetrySuccesses,
			},
			"messageRetry": map[string]interface{}{
				"attempts":  metrics.Retry.MessageRetryAttempts,
				"successes": metrics.Retry.MessageRetrySuccesses,
			},
			"dlqMessages":     metrics.Retry.DLQMessages,
			"expiredMessages": metrics.Retry.ExpiredMessages,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"transportRetry": map[string]interface{}{},
		"messageRetry":   map[string]interface{}{},
	})
}

func (s *Server) handleMetricsFlowControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	metrics := s.bridge.Metrics()
	if metrics != nil {
		writeJSON(w, http.StatusOK, metrics.FlowControl)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"currentInFlight":    0,
		"maxInFlight":        100,
		"backpressureEvents": 0,
	})
}

// ============================================================================
// Tracing Handlers
// ============================================================================

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	// Traces would need trace storage integration
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"traces":  []interface{}{},
		"total":   0,
		"hasMore": false,
	})
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	prefix := s.config.MonitorAPIPrefix + "/traces/"
	traceID := extractPathParam(r, prefix)

	// Handle spans subpath
	if strings.HasSuffix(traceID, "/spans") {
		traceID = strings.TrimSuffix(traceID, "/spans")
		s.handleTraceSpans(w, r, traceID)
		return
	}

	writeError(w, http.StatusNotFound, "NOT_FOUND", "trace not found")
}

func (s *Server) handleTraceSpans(w http.ResponseWriter, r *http.Request, traceID string) {
	writeJSON(w, http.StatusOK, []interface{}{})
}

func (s *Server) handleTracesSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"traces":  []interface{}{},
		"total":   0,
		"hasMore": false,
	})
}

func (s *Server) handleTracesConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled":      true,
			"samplingRate": 1.0,
			"exporters":    []interface{}{},
			"propagators":  []string{"tracecontext", "baggage"},
		})

	case http.MethodPut:
		var req struct {
			Enabled      *bool    `json:"enabled"`
			SamplingRate *float64 `json:"samplingRate"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled":      true,
			"samplingRate": 1.0,
		})

	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

func (s *Server) handleTracesExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"exporters": []interface{}{},
	})
}

// ============================================================================
// Instance Handlers
// ============================================================================

func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	// For standalone, return just this instance
	instances := []map[string]interface{}{
		{
			"id":               s.bridge.GetID(),
			"clusterId":        s.bridge.GetClusterID(),
			"status":           instanceStatus(s.bridge.IsReady(), s.bridge.IsLive()),
			"pipelinesCount":   len(s.bridge.GetPipelines()),
			"connectionsCount": len(s.bridge.ListConnections()),
		},
	}

	writeJSON(w, http.StatusOK, instances)
}

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	prefix := s.config.MonitorAPIPrefix + "/instances/"
	id := extractPathParam(r, prefix)

	// Handle subpaths
	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		id = parts[0]
		subpath := parts[1]

		switch subpath {
		case "metrics":
			s.handleInstanceMetrics(w, r, id)
		case "logs":
			s.handleInstanceLogs(w, r, id)
		default:
			writeError(w, http.StatusNotFound, "NOT_FOUND", "endpoint not found")
		}
		return
	}

	if id != s.bridge.GetID() {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "instance not found")
		return
	}

	ctx := r.Context()
	health := s.bridge.Health(ctx)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":               s.bridge.GetID(),
		"clusterId":        s.bridge.GetClusterID(),
		"status":           instanceStatus(s.bridge.IsReady(), s.bridge.IsLive()),
		"pipelinesCount":   len(s.bridge.GetPipelines()),
		"connectionsCount": len(s.bridge.ListConnections()),
		"health":           health,
	})
}

func (s *Server) handleInstanceMetrics(w http.ResponseWriter, r *http.Request, id string) {
	if id != s.bridge.GetID() {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "instance not found")
		return
	}

	metrics := s.bridge.Metrics()
	writeJSON(w, http.StatusOK, metrics)
}

func (s *Server) handleInstanceLogs(w http.ResponseWriter, r *http.Request, id string) {
	if id != s.bridge.GetID() {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "instance not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": []interface{}{},
	})
}

// ============================================================================
// Cluster Handlers
// ============================================================================

func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	clusterID := s.bridge.GetClusterID()
	if clusterID == "" {
		clusterID = "standalone"
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"clusterId":        clusterID,
		"status":           "healthy",
		"instanceCount":    1,
		"healthyInstances": 1,
		"leaderElected":    true,
		"leaderId":         s.bridge.GetID(),
	})
}

func (s *Server) handleClusterTopology(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"clusterId": s.bridge.GetClusterID(),
		"nodes": []map[string]interface{}{
			{
				"id":     s.bridge.GetID(),
				"status": instanceStatus(s.bridge.IsReady(), s.bridge.IsLive()),
			},
		},
		"edges": []interface{}{},
	})
}

func (s *Server) handleClusterMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	metrics := s.bridge.Metrics()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"clusterId": s.bridge.GetClusterID(),
		"instanceMetrics": map[string]interface{}{
			s.bridge.GetID(): metrics,
		},
	})
}

func (s *Server) handleClusterLeader(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"leaderId":  s.bridge.GetID(),
		"electedAt": time.Now().UTC().Format(time.RFC3339),
	})
}

// ============================================================================
// Alerts Handlers
// ============================================================================

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, []interface{}{})
}

func (s *Server) handleAlert(w http.ResponseWriter, r *http.Request) {
	prefix := s.config.MonitorAPIPrefix + "/alerts/"
	id := extractPathParam(r, prefix)

	// Handle acknowledge action
	if strings.HasSuffix(id, "/acknowledge") {
		s.handleAlertAcknowledge(w, r, strings.TrimSuffix(id, "/acknowledge"))
		return
	}

	writeError(w, http.StatusNotFound, "NOT_FOUND", "alert not found")
}

func (s *Server) handleAlertAcknowledge(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"acknowledged": true,
	})
}

func (s *Server) handleAlertRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, []interface{}{})

	case http.MethodPost:
		var req struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
			Severity   string `json:"severity"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"id":         "rule-1",
			"name":       req.Name,
			"expression": req.Expression,
			"severity":   req.Severity,
			"enabled":    true,
		})

	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

// ============================================================================
// WebSocket Streaming Handlers - Implemented in websocket.go
// ============================================================================

// ============================================================================
// Helper Functions
// ============================================================================

func liveMessage(live bool) string {
	if live {
		return "bridge is alive"
	}
	return "bridge is not alive"
}

func readyMessage(ready bool) string {
	if ready {
		return "bridge is ready"
	}
	return "bridge is not ready"
}

func instanceStatus(ready, live bool) string {
	if !live {
		return "unhealthy"
	}
	if !ready {
		return "degraded"
	}
	return "healthy"
}

// Ensure BridgeController is satisfied by checking we use all methods.
var _ = func() bool {
	var bc BridgeController
	_ = bc.Start
	_ = bc.Stop
	_ = bc.IsReady
	_ = bc.IsLive
	_ = bc.Health
	_ = bc.Metrics
	_ = bc.GetPipelines
	_ = bc.GetPipeline
	_ = bc.AddPipelineRunning
	_ = bc.RemovePipelineRunning
	_ = bc.GetConnection
	_ = bc.ListConnections
	_ = bc.AddConnection
	_ = bc.Validate
	_ = bc.GetID
	_ = bc.GetClusterID
	// Prevent unused variable warning
	var _ context.Context
	return true
}()
