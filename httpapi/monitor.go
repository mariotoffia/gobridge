package httpapi

import (
	"net/http"

	"github.com/mariotoffia/gobridge/ports"
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
	rt := s.currentRuntime()
	w.Header().Set("Cache-Control", "no-cache, max-age=0")

	// This probe is UNAUTHENTICATED (for LBs/orchestrators). It therefore
	// exposes only a coarse status string and the HTTP status code — never
	// instance_id, route count, or component-failure details, which are
	// operational reconnaissance and belong behind auth (see /deephealth).
	if rt == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable",
		})
		return
	}

	status := "ok"
	httpStatus := http.StatusOK
	if !rt.Healthy() {
		status = "unhealthy"
		httpStatus = http.StatusServiceUnavailable
	} else if !rt.IsRunning() {
		status = "not_running"
		httpStatus = http.StatusServiceUnavailable
	}
	writeJSON(w, httpStatus, map[string]any{"status": status})
}

// handleLive reports process liveness. It returns 200 while the process is
// alive and able to recover — including during runtime swap windows when the
// runtime is temporarily nil, AND after a deliberate admin stop (a clean pause
// leaves the runtime non-terminal) — and 503 only when the runtime is terminal:
// an unrecoverable component failure that cancelled the runtime. Kubernetes uses
// this probe to restart the container, so failing closed on a terminal runtime
// is what turns a dead-but-running process into an automatic restart, while a
// deliberate pause must NOT be mistaken for death (CRITICAL 1 / CRITICAL 3).
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, max-age=0")
	if rt := s.currentRuntime(); rt != nil && rt.Terminal() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "terminal"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

// handleReady reports whether the runtime has reached the requested
// readiness level. The level is taken from the ?level= query parameter:
//
//	?level=running     — runtime started + healthy
//	?level=connected   — all sessions connected to broker
//	?level=subscribed  — all SUBSCRIBE frames acknowledged by broker
//	?level=full        — all routes have handler registered (ServiceLevelFull)
//
// Operators map probes to levels:
//   - K8s liveness:   /live (200 while recoverable, 503 once terminal)
//   - K8s readiness:  /ready?level=connected (tolerates intermittent broker hiccups)
//   - Pre-traffic:    /ready?level=full (strict, every route ready to dispatch)
//
// When ?level= is absent, the legacy contract applies: 200 with
// {status, role} when the instance is ready for traffic, 503 with
// {error: "not ready"} otherwise. With ?level= the response is the
// structured form {status, role, level, requested}, returning 503 when
// have<want.
//
// HIGH-3: the legacy (no ?level=) default now requires LevelFull — every
// session subscribed AND every route ready to dispatch — rather than the old
// running+healthy check. running+healthy stayed green while an ISOLATED route or
// session was permanently faulting (superviseRoute/superviseSession record
// per-component faults without flipping the global healthy flag), so a pod kept
// taking traffic it could not serve. LevelFull is the conservative rung on the
// readiness ladder that means "actually able to serve every route"; an isolated
// route fault caps the achieved level at LevelSubscribed (< LevelFull) → 503.
// A standby instance is capped at LevelSubscribed by design, so the legacy probe
// returns 503 for a standby (it holds no lease and dispatches nothing) — use
// ?level=connected/subscribed for standby-tolerant probes. The explicit
// ?level= contract (running/connected/subscribed/full) is unchanged.
//
// Unknown levels return 400 Bad Request.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	// Readiness must never be cached — set no-store BEFORE any branch so the
	// nil-runtime 503 carries it too; a cached "ready" would keep an LB routing
	// traffic at an instance that has since gone unready.
	w.Header().Set("Cache-Control", "no-cache, max-age=0")
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}

	rawLevel := r.URL.Query().Get("level")
	if rawLevel == "" {
		// Legacy path: preserve historical {status,role} / {error} shape, but
		// gate on the achieved readiness level (HIGH-3) so an isolated route or
		// session fault sheds traffic instead of advertising a false green.
		if rt.ReadinessLevel(r.Context()) < ports.LevelFull {
			writeErr(w, http.StatusServiceUnavailable, "not ready")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ready",
			"role":   rt.Role(),
		})
		return
	}

	want, ok := ports.ParseReadinessLevel(rawLevel)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid level (want one of: live, running, connected, subscribed, full)")
		return
	}
	have := rt.ReadinessLevel(r.Context())
	resp := map[string]any{
		"status":    "ready",
		"role":      rt.Role(),
		"level":     have.String(),
		"requested": want.String(),
	}
	if have < want {
		resp["status"] = "not_ready"
		writeJSON(w, http.StatusServiceUnavailable, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Authenticated sensitive endpoints ---

// topologyRouteView is a compact route projection for topology responses.
type topologyRouteView struct {
	ID           string `json:"id"`
	DeliveryMode string `json:"delivery_mode"`
	DispatchMode string `json:"dispatch_mode"`
}

// configVersion reports the optimistic-concurrency version of the
// running configuration and whether a config source is wired. It is
// the scrapeable surface for fleet-convergence monitoring: when
// several instances share one config file, an instance whose
// config_version lags the others has not yet converged on the latest
// committed configuration. It returns ok=false when no ConfigProvider
// is configured, so callers omit the field rather than report a
// misleading zero (which otherwise means "never committed via the API").
func (s *Server) configVersion() (version int, ok bool) {
	if s.cfg.ConfigProvider == nil {
		return 0, false
	}
	cfg := s.cfg.ConfigProvider()
	if cfg == nil {
		return 0, false
	}
	return cfg.Version, true
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	routes := rt.Routes()
	views := make([]topologyRouteView, len(routes))
	for i, ri := range routes {
		views[i] = topologyRouteView{
			ID:           ri.ID,
			DeliveryMode: string(ri.DeliveryMode),
			DispatchMode: string(ri.DispatchMode),
		}
	}
	resp := map[string]any{
		"instance_id": rt.InstanceID(),
		"running":     rt.IsRunning(),
		"routes":      views,
	}
	// Surface the running config version for fleet-convergence
	// monitoring when a config provider is wired; omit it otherwise so
	// a missing provider is not confused with config version 0.
	if version, ok := s.configVersion(); ok {
		resp["config_version"] = version
	}
	writeJSON(w, http.StatusOK, resp)
}

// monitorRouteView is a detailed route view with policy fields.
type monitorRouteView struct {
	ID            string `json:"id"`
	DeliveryMode  string `json:"delivery_mode"`
	DispatchMode  string `json:"dispatch_mode"`
	MaxInFlight   int    `json:"max_in_flight"`
	MaxReplay     int    `json:"max_replay"`
	AckAfter      string `json:"ack_after"`
	OnExpired     string `json:"on_expired"`
	OnPermFailure string `json:"on_perm_failure"`
	OnFiltered    string `json:"on_filtered"`
}

func (s *Server) handleMonitorRoutes(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	routes := rt.Routes()
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
			OnFiltered:    string(ri.Policy.OnFiltered),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": views})
}

// deepHealthResponse is the JSON-serializable representation of a deep
// health check. It mirrors ports.DeepHealth with explicit JSON tags.
type deepHealthResponse struct {
	Running         bool                        `json:"running"`
	Healthy         bool                        `json:"healthy"`
	InstanceID      string                      `json:"instance_id"`
	Role            string                      `json:"role"`
	ReadyForTraffic bool                        `json:"ready_for_traffic"`
	ServiceLevel    string                      `json:"service_level"`
	Level           string                      `json:"level"` // current ReadinessLevel
	Sessions        []deepHealthSessionResponse `json:"sessions"`
	Routes          []deepHealthRouteResponse   `json:"routes"`
	// ConfigWatch surfaces live-reconfiguration health so operators can see a
	// bridge running blind on its last good config (degraded config-watch was
	// previously invisible outside the logs). Omitted when no DegradedProvider
	// is wired; additive to the existing fields.
	ConfigWatch *ConfigWatchHealth `json:"config_watch,omitempty"`
}

// ConfigWatchHealth is the deep-health projection of live-reconfiguration state.
type ConfigWatchHealth struct {
	Degraded           bool   `json:"degraded"`
	Reason             string `json:"reason,omitempty"`
	ReconfigurePending bool   `json:"reconfigure_pending"`
	DesiredVersion     *int   `json:"desired_version,omitempty"`
	RunningVersion     *int   `json:"running_version,omitempty"`
	LastApplyError     string `json:"last_apply_error,omitempty"`
	// Rollout surfaces the coordinated cluster rollout barrier as this member
	// last observed it. Omitted entirely unless the deployment runs one
	// (bridge.cluster.rollout: coordinated with a barrier wired). It answers the
	// question no per-node signal can: a healthy member whose config change is
	// stuck behind a peer that has not acknowledged.
	Rollout *ClusterRolloutHealth `json:"rollout,omitempty"`
}

// ClusterRolloutHealth is the deep-health projection of the coordinated cluster
// rollout barrier (design cluster-config-rollout-protocol.md §9).
type ClusterRolloutHealth struct {
	// MemberID is this node's identity within the cohort roster.
	MemberID string `json:"member_id"`
	// Generation and State describe the observed rollout; State is empty when
	// no rollout has ever been proposed in this cohort.
	Generation uint64 `json:"generation"`
	State      string `json:"state,omitempty"`
	// ConfigVersion is the config version the rollout carries.
	ConfigVersion int `json:"config_version"`
	// Epoch is the frozen membership epoch; Acked and Nacked are who has voted.
	// Epoch minus Acked minus Nacked is exactly who the cohort is waiting for.
	Epoch  []string `json:"epoch,omitempty"`
	Acked  []string `json:"acked,omitempty"`
	Nacked []string `json:"nacked,omitempty"`
	// Reason carries the abort reason or nack aggregation when present.
	Reason string `json:"reason,omitempty"`
	// CandidateStaged reports whether THIS member has received the candidate
	// config from its own config source. False on a long-staging rollout
	// identifies this member as the one holding the cohort up.
	CandidateStaged bool `json:"candidate_staged"`
	// Applied reports whether this member is actually RUNNING the generation.
	// Meaningful once State is "committed", where it is the signal that
	// separates a healthy cohort from a split one: the rollout row reads
	// committed on every member, but a member whose local swap failed is still
	// on the previous generation. Alert on state=committed AND applied=false.
	Applied bool `json:"applied"`
}

type deepHealthSessionResponse struct {
	SessionID                string   `json:"session_id"`
	Connected                bool     `json:"connected"`
	HasLease                 bool     `json:"has_lease"`
	SubscriptionsWanted      int      `json:"subscriptions_wanted"`
	SubscriptionsActive      int      `json:"subscriptions_active"`
	ActiveTopics             []string `json:"active_topics,omitempty"`
	Ready                    bool     `json:"ready"`
	ServiceLevel             string   `json:"service_level"`
	UnsettledCount           int      `json:"unsettled_count"`
	OldestUnsettledAgeMS     int64    `json:"oldest_unsettled_age_ms"`
	ReceiveWindowUtilization float64  `json:"receive_window_utilization"`
	RecoveryRecycleCount     uint64   `json:"recovery_recycle_count"`
}

type deepHealthRouteResponse struct {
	ID           string `json:"id"`
	DeliveryMode string `json:"delivery_mode"`
	Ready        bool   `json:"ready"`      // route runner started + receiver started
	InFlight     int    `json:"in_flight"`  // currently-processing delivery count
	RouteDead    bool   `json:"route_dead"` // route wedged flapping at the supervisor backoff cap (F5)
}

func (s *Server) handleDeepHealth(w http.ResponseWriter, r *http.Request) {
	rt := s.currentRuntime()
	if rt == nil {
		writeErr(w, http.StatusServiceUnavailable, "runtime not available")
		return
	}
	dh := rt.DeepHealth(r.Context())

	resp := deepHealthResponse{
		Running:         dh.Running,
		Healthy:         dh.Healthy,
		InstanceID:      dh.InstanceID,
		Role:            dh.Role,
		ReadyForTraffic: dh.ReadyForTraffic,
		ServiceLevel:    string(dh.ServiceLevel),
		// Derive the readiness level from the SAME snapshot rather than calling
		// rt.ReadinessLevel (which takes a second, independent DeepHealth sweep):
		// one snapshot keeps Level internally consistent with the Sessions/Routes
		// rendered below and halves the health-probe cost.
		Level: ports.ReadinessLevelFromDeepHealth(dh).String(),
	}

	resp.Sessions = make([]deepHealthSessionResponse, len(dh.Sessions))
	for i, sh := range dh.Sessions {
		resp.Sessions[i] = deepHealthSessionResponse{
			SessionID:                sh.SessionID,
			Connected:                sh.Connected,
			HasLease:                 sh.HasLease,
			SubscriptionsWanted:      sh.SubscriptionsWanted,
			SubscriptionsActive:      sh.SubscriptionsActive,
			ActiveTopics:             sh.ActiveTopics,
			Ready:                    sh.Ready,
			ServiceLevel:             string(sh.ServiceLevel),
			UnsettledCount:           sh.UnsettledCount,
			OldestUnsettledAgeMS:     sh.OldestUnsettledAge.Milliseconds(),
			ReceiveWindowUtilization: sh.ReceiveWindowUtilization,
			RecoveryRecycleCount:     sh.RecoveryRecycleCount,
		}
	}

	resp.Routes = make([]deepHealthRouteResponse, len(dh.Routes))
	for i, rh := range dh.Routes {
		resp.Routes[i] = deepHealthRouteResponse{
			ID:           rh.ID,
			DeliveryMode: rh.DeliveryMode,
			Ready:        rh.Ready,
			InFlight:     rh.InFlight,
			RouteDead:    rh.RouteDead,
		}
	}

	status := http.StatusOK
	if !dh.ReadyForTraffic {
		status = http.StatusServiceUnavailable
	}

	// Surface live-reconfiguration health additively when a provider is wired.
	// A degraded config-watch does NOT flip the traffic status: the runtime
	// keeps serving its last good config, so failing readiness here would pull a
	// healthy bridge out of rotation over a config-observation problem.
	if s.cfg.ConfigWatchProvider != nil {
		status := s.cfg.ConfigWatchProvider()
		resp.ConfigWatch = &status
	} else if s.cfg.DegradedProvider != nil {
		degraded, reason := s.cfg.DegradedProvider()
		resp.ConfigWatch = &ConfigWatchHealth{Degraded: degraded, Reason: reason}
	}

	writeJSON(w, status, resp)
}
