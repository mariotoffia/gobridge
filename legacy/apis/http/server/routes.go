package server

import (
	"net/http"
)

// setupRoutes configures all API routes.
func (s *Server) setupRoutes() {
	if s.config.EnableAdmin {
		s.setupAdminRoutes()
	}

	if s.config.EnableMonitor {
		s.setupMonitorRoutes()
	}
}

// setupAdminRoutes configures admin API routes.
func (s *Server) setupAdminRoutes() {
	prefix := s.config.AdminAPIPrefix

	// Bridge lifecycle
	s.adminMux.HandleFunc(prefix+"/bridge", s.authMiddleware(s.handleBridge))
	s.adminMux.HandleFunc(prefix+"/bridge/start", s.authMiddleware(s.handleBridgeStart))
	s.adminMux.HandleFunc(prefix+"/bridge/stop", s.authMiddleware(s.handleBridgeStop))
	s.adminMux.HandleFunc(prefix+"/bridge/drain", s.authMiddleware(s.handleBridgeDrain))

	// Connections
	s.adminMux.HandleFunc(prefix+"/connections", s.authMiddleware(s.handleConnections))
	s.adminMux.HandleFunc(prefix+"/connections/", s.authMiddleware(s.handleConnection))

	// Pipelines
	s.adminMux.HandleFunc(prefix+"/pipelines", s.authMiddleware(s.handlePipelines))
	s.adminMux.HandleFunc(prefix+"/pipelines/", s.authMiddleware(s.handlePipeline))

	// Routes
	s.adminMux.HandleFunc(prefix+"/routes", s.authMiddleware(s.handleRoutes))
	s.adminMux.HandleFunc(prefix+"/routes/", s.authMiddleware(s.handleRoute))

	// DLQ
	s.adminMux.HandleFunc(prefix+"/dlq", s.authMiddleware(s.handleDLQ))
	s.adminMux.HandleFunc(prefix+"/dlq/messages", s.authMiddleware(s.handleDLQMessages))
	s.adminMux.HandleFunc(prefix+"/dlq/messages/", s.authMiddleware(s.handleDLQMessage))
	s.adminMux.HandleFunc(prefix+"/dlq/replay", s.authMiddleware(s.handleDLQReplay))
	s.adminMux.HandleFunc(prefix+"/dlq/purge", s.authMiddleware(s.handleDLQPurge))

	// Config
	s.adminMux.HandleFunc(prefix+"/config", s.authMiddleware(s.handleConfig))
	s.adminMux.HandleFunc(prefix+"/config/reload", s.authMiddleware(s.handleConfigReload))
	s.adminMux.HandleFunc(prefix+"/config/validate", s.authMiddleware(s.handleConfigValidate))
	s.adminMux.HandleFunc(prefix+"/config/diff", s.authMiddleware(s.handleConfigDiff))

	// Testing
	s.adminMux.HandleFunc(prefix+"/test/message", s.authMiddleware(s.handleTestMessage))
	s.adminMux.HandleFunc(prefix+"/test/message/batch", s.authMiddleware(s.handleTestMessageBatch))
	s.adminMux.HandleFunc(prefix+"/test/pipeline/", s.authMiddleware(s.handleTestPipeline))

	// Diagnostics
	s.adminMux.HandleFunc(prefix+"/diagnostics/logs", s.authMiddleware(s.handleDiagnosticsLogs))
	s.adminMux.HandleFunc(prefix+"/diagnostics/errors", s.authMiddleware(s.handleDiagnosticsErrors))
	s.adminMux.HandleFunc(prefix+"/diagnostics/debug", s.authMiddleware(s.handleDiagnosticsDebug))
}

// setupMonitorRoutes configures monitor API routes.
func (s *Server) setupMonitorRoutes() {
	prefix := s.config.MonitorAPIPrefix

	// Health checks (no auth required)
	s.monitorMux.HandleFunc(prefix+"/health", s.handleHealth)
	s.monitorMux.HandleFunc(prefix+"/health/live", s.handleHealthLive)
	s.monitorMux.HandleFunc(prefix+"/health/ready", s.handleHealthReady)
	s.monitorMux.HandleFunc(prefix+"/health/startup", s.handleHealthStartup)

	// Metrics (no auth required)
	s.monitorMux.HandleFunc(prefix+"/metrics", s.handleMetrics)
	s.monitorMux.HandleFunc(prefix+"/metrics/json", s.handleMetricsJSON)
	s.monitorMux.HandleFunc(prefix+"/metrics/pipelines", s.handleMetricsPipelines)
	s.monitorMux.HandleFunc(prefix+"/metrics/pipelines/", s.handleMetricsPipeline)
	s.monitorMux.HandleFunc(prefix+"/metrics/connections", s.handleMetricsConnections)
	s.monitorMux.HandleFunc(prefix+"/metrics/retry", s.handleMetricsRetry)
	s.monitorMux.HandleFunc(prefix+"/metrics/flow-control", s.handleMetricsFlowControl)

	// Tracing
	s.monitorMux.HandleFunc(prefix+"/traces", s.handleTraces)
	s.monitorMux.HandleFunc(prefix+"/traces/search", s.handleTracesSearch)
	s.monitorMux.HandleFunc(prefix+"/traces/config", s.handleTracesConfig)
	s.monitorMux.HandleFunc(prefix+"/traces/export", s.handleTracesExport)
	s.monitorMux.HandleFunc(prefix+"/traces/", s.handleTrace)

	// Instances
	s.monitorMux.HandleFunc(prefix+"/instances", s.handleInstances)
	s.monitorMux.HandleFunc(prefix+"/instances/", s.handleInstance)

	// Cluster
	s.monitorMux.HandleFunc(prefix+"/cluster", s.handleCluster)
	s.monitorMux.HandleFunc(prefix+"/cluster/topology", s.handleClusterTopology)
	s.monitorMux.HandleFunc(prefix+"/cluster/metrics", s.handleClusterMetrics)
	s.monitorMux.HandleFunc(prefix+"/cluster/leader", s.handleClusterLeader)

	// Alerts
	s.monitorMux.HandleFunc(prefix+"/alerts", s.handleAlerts)
	s.monitorMux.HandleFunc(prefix+"/alerts/rules", s.handleAlertRules)
	s.monitorMux.HandleFunc(prefix+"/alerts/", s.handleAlert)

	// WebSocket streaming
	s.monitorMux.HandleFunc(prefix+"/stream/metrics", s.handleStreamMetrics)
	s.monitorMux.HandleFunc(prefix+"/stream/logs", s.handleStreamLogs)
	s.monitorMux.HandleFunc(prefix+"/stream/traces", s.handleStreamTraces)
}

// Helper to extract path parameter
func extractPathParam(r *http.Request, prefix string) string {
	path := r.URL.Path
	if len(path) > len(prefix) {
		return path[len(prefix):]
	}
	return ""
}

// Method routing helper
func methodRouter(w http.ResponseWriter, r *http.Request, handlers map[string]http.HandlerFunc) {
	if handler, ok := handlers[r.Method]; ok {
		handler(w, r)
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
}
