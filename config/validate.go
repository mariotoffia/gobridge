package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/validate"
)

// ValidationError is the shape the config package returns from
// Validate / ValidateWithWarnings. It is an alias for the
// transport-neutral ports.BlueprintValidationError so admin layers
// (httpapi) can inspect Warnings/Errors without importing this
// package.
type ValidationError = ports.BlueprintValidationError

// Validate performs structural validation on a ports.BridgeConfig. It
// checks required fields, valid enum values, and duration formats at
// the field level, then delegates cross-reference and route-graph
// validation (FK integrity between sessions, receivers, senders,
// bindings, routes, resolver bindings, and clustered MQTT shared-
// subscription rules) to the validate package.
//
// It does not check transport-specific options.
func Validate(cfg *ports.BridgeConfig) error {
	ve := validateConfig(cfg)
	if ve.HasErrors() {
		return ve
	}
	return nil
}

// ValidateWithWarnings performs Validate and returns any non-fatal
// warnings (e.g. direct_hold fencing advisory) even when validation
// passes. The caller should log warnings but not treat them as errors.
func ValidateWithWarnings(cfg *ports.BridgeConfig) (warnings []string, err error) {
	ve := validateConfig(cfg)
	if ve.HasErrors() {
		return ve.Warnings, ve
	}
	return ve.Warnings, nil
}

func validateConfig(cfg *ports.BridgeConfig) *ValidationError {
	ve := &ValidationError{}

	validateBridgeFields(ve, cfg)
	validateConfigWatchFields(ve, cfg)

	if graphErr := validate.ValidateBlueprintGraph(cfg); graphErr != nil {
		ve.Errors = append(ve.Errors, graphErr.Errors...)
		ve.Warnings = append(ve.Warnings, graphErr.Warnings...)
	}

	validateStaleClaimDuration(ve, cfg)
	validateSessionRenewTiming(ve, cfg)
	validateFailoverFields(ve, cfg)
	validateConnectLeaseBudget(ve, cfg)
	validateClusterEndpoints(ve, cfg)
	validateClusterRollout(ve, cfg)
	validateClusteredExclusiveHTTPDirectHold(ve, cfg)

	return ve
}

// rolloutModeCoordinated opts a clustered deployment into the coordinated
// rollout barrier (docs/design/cluster-config-rollout-protocol.md §8). Any other
// value keeps ADR 0012's refuse-live-reconfig behavior.
const rolloutModeCoordinated = "coordinated"

// rolloutModeRefuse is the explicit spelling of the default.
const rolloutModeRefuse = "refuse"

// validateClusterRollout admits a coordinated-rollout deployment only when its
// barrier can actually function. Each rule below rejects a shape that would fail
// LATER — at the first live reload, or worse, silently:
//
//   - An unknown bridge.cluster.rollout value must not silently mean "refuse".
//     A typo ("cordinated") would leave an operator believing coordinated
//     rollout is on while every change is refused.
//   - coordinated on a NON-clustered deployment is meaningless: there is no
//     cohort to coordinate, and the guard never consults the barrier.
//   - coordinated REQUIRES a non-empty bridge.cluster.members roster. The roster
//     is the membership epoch the barrier freezes at Propose and counts acks
//     against (I2); with an empty epoch every ack set vacuously covers it, so the
//     first coordinator observation would commit a config no member validated.
//   - A duplicate member id is rejected rather than deduped: it means the roster
//     was written by hand against a real cohort and one id is wrong, and the
//     resulting epoch would be smaller than the operator believes — a barrier
//     that commits without one node's ack.
//
// NOT checked here, deliberately: §2 N3's "coordinated requires the versioned,
// CAS-capable config source (not file/EFS)" and "this node's member id appears
// in the roster". Neither the source implementation nor this process's member id
// is part of the blueprint — both are composition-root wiring — so the blueprint
// validator cannot see them. They are enforced where they ARE visible: a root
// that wires no rollout store makes coordinated mode fail closed on every reload
// (bridge.proposeCoordinatedRollout), and the member-id membership check runs at
// startup in the Supervisor's coordinated preflight.
func validateClusterRollout(ve *ValidationError, cfg *ports.BridgeConfig) {
	c := cfg.Bridge.Cluster
	if c == nil {
		return
	}
	switch c.Rollout {
	case "", rolloutModeRefuse:
		// today's behavior; the roster is unused. A confirm window here is a
		// configuration error: there is no barrier to confirm.
		if c.ConfirmWindow != "" {
			ve.Addf("bridge.cluster.confirm_window: only valid when bridge.cluster.rollout is %q; the "+
				"refuse/standalone paths have no rollout barrier to confirm (design §8.1)", rolloutModeCoordinated)
		}
		return
	case rolloutModeCoordinated:
	default:
		ve.Addf("bridge.cluster.rollout: %q is not a valid value; use %q (the default: refuse every "+
			"live reload of a clustered deployment, ADR 0012) or %q (coordinated rollout barrier). An "+
			"unrecognised value must not be silently treated as %q",
			c.Rollout, rolloutModeRefuse, rolloutModeCoordinated, rolloutModeRefuse)
		return
	}
	if !deploymentIsClustered(cfg) {
		ve.Addf("bridge.cluster.rollout: %q requires a clustered deployment (bridge.deployment_mode: "+
			"clustered), because it coordinates a config change across a cohort; a standalone bridge "+
			"reloads directly and has no cohort to coordinate", rolloutModeCoordinated)
	}
	if len(c.Members) == 0 {
		ve.Addf("bridge.cluster.members: required and non-empty when bridge.cluster.rollout is %q. It "+
			"is the cohort roster the rollout barrier freezes as its membership epoch and counts "+
			"acknowledgements against; with an empty roster the barrier would be satisfied by ZERO "+
			"acknowledgements and could commit a config no member validated. List every member id, "+
			"e.g. members: [node-a, node-b, node-c]. NOTE: this is NOT bridge.cluster.endpoints, which "+
			"advertises THIS instance's capability endpoints", rolloutModeCoordinated)
		return
	}
	seen := make(map[string]struct{}, len(c.Members))
	for i, id := range c.Members {
		if id == "" {
			ve.Addf("bridge.cluster.members[%d]: empty member id; every cohort member needs a stable, "+
				"distinct id matching the id its process announces to the rollout barrier", i)
			continue
		}
		if _, dup := seen[id]; dup {
			ve.Addf("bridge.cluster.members[%d]: duplicate member id %q. The roster is the membership "+
				"epoch the barrier counts acknowledgements against, so a duplicate makes the cohort "+
				"look smaller than it is and the barrier could commit without one member's "+
				"acknowledgement", i, id)
		}
		seen[id] = struct{}{}
	}
	validateConfirmWindow(ve, c.ConfirmWindow)
}

// validateConfirmWindow rejects a malformed or non-positive confirm window on a
// coordinated cohort (design §8.1). Empty is fine (the base protocol). A parse
// failure or a value <= 0 must not silently disable the window — an operator who
// wrote confirm_window expects a provisional apply, and a silent zero would give
// them the base protocol under a different name.
func validateConfirmWindow(ve *ValidationError, raw string) {
	if raw == "" {
		return
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		ve.Addf("bridge.cluster.confirm_window: %q is not a valid Go duration (e.g. \"90s\", \"2m\"): %v", raw, err)
		return
	}
	if d <= 0 {
		ve.Addf("bridge.cluster.confirm_window: %q must be positive; use a non-zero window to enable the "+
			"provisional-apply confirm window, or omit it for the base protocol", raw)
	}
}

// deploymentIsClustered mirrors the bridge builder's clustered predicate
// (bridge/builder_prepare.go): a deployment is clustered when deployment_mode is
// "clustered" OR a static cluster.endpoints override is present. The config-layer
// cluster rules use it so they reject exactly the set the runtime treats as
// clustered.
func validateFailoverFields(ve *ValidationError, cfg *ports.BridgeConfig) {
	const maxStartupAllowance = 10 * time.Minute
	for _, route := range cfg.Routes {
		if route.Session == nil {
			continue
		}
		if raw := route.Session.FailoverSLO; raw != "" {
			duration, err := time.ParseDuration(raw)
			if err != nil || duration <= 0 {
				ve.Addf("routes[%s].session.failover_slo: must be a positive duration, got %q", route.ID, raw)
			}
		}
		if raw := route.Session.StartupAllowance; raw != "" {
			duration, err := time.ParseDuration(raw)
			if err != nil || duration < 0 {
				ve.Addf("routes[%s].session.startup_allowance: must be a non-negative duration, got %q", route.ID, raw)
			} else if duration > maxStartupAllowance {
				ve.Addf("routes[%s].session.startup_allowance: %s exceeds maximum %s", route.ID, duration, maxStartupAllowance)
			}
		}
	}
}

func deploymentIsClustered(cfg *ports.BridgeConfig) bool {
	return ports.IsClusteredDeployment(cfg)
}

// validateClusterEndpoints rejects the copied-from-docs peer-membership shape of
// cluster.endpoints at load time instead of at forward time (finding HIGH-1).
// cluster.endpoints advertises THIS instance's CAPABILITY endpoints keyed by
// capability (e.g. http -> "http://host:port"); the HTTP forwarder looks up
// target.Endpoints["http"] to forward a remote exclusive request
// (adapters/http/transport/forwarder.go). A peer map
// ({instance-01: "10.0.1.10:8080"}) has instance-id keys and no "http" key, so a
// non-owner forwarding an exclusive HTTP request gets "target has no HTTP
// endpoint" -> 502 for every remote ingress.
//
// ponytail: this is the finding's narrower fallback. Rather than wire an
// "exclusive route reachable over HTTP transport" detector, whenever
// cluster.endpoints is STATICALLY set in a clustered deployment we require a
// valid "http" capability entry. A peer-map shape has instance keys (not "http")
// and fails, which is the exact mistake we reject; a correct
// {http: "http://host:port"} passes; leaving endpoints unset (auto-discovery) is
// NOT forced.
func validateClusterEndpoints(ve *ValidationError, cfg *ports.BridgeConfig) {
	if cfg.Bridge.Cluster == nil || len(cfg.Bridge.Cluster.Endpoints) == 0 {
		return // auto-discovery: no static override to validate
	}
	if !deploymentIsClustered(cfg) {
		return
	}
	raw, ok := cfg.Bridge.Cluster.Endpoints["http"]
	if !ok {
		ve.Addf("bridge.cluster.endpoints: statically set in a clustered deployment but has no " +
			"\"http\" capability key. cluster.endpoints advertises THIS instance's capability " +
			"endpoints keyed by capability (e.g. http: \"http://host:port\"), NOT a peer/instance " +
			"map — a peer map (instance-01: \"10.0.1.10:8080\") has no \"http\" key, so every remote " +
			"exclusive HTTP request fails to forward (502). Set endpoints: {http: " +
			"\"http://<this-instance-host>:<port>\"}, or remove the static override to use " +
			"auto-discovery")
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		ve.Addf("bridge.cluster.endpoints.http: %q is not an absolute http(s)://host:port URL; the "+
			"HTTP forwarder POSTs directly to this value, so it must be a full URL "+
			"(e.g. \"http://10.0.1.10:8080\"), not a bare host:port or an instance address", raw)
	}
}

// validateClusteredExclusiveHTTPDirectHold rejects direct_hold delivery for a
// clustered exclusive route whose ingress is the HTTP transport (finding
// HIGH-4). Forwarded trusted HTTP requests deliberately skip the ownership
// re-check (adapters/http/transport/receiver.go), and direct_hold sends straight
// from the sender boundary with NO lease/fencing token
// (runtime/route/runner.go), so a request forwarded to an owner that has just
// stepped down can be sent by the OLD owner while the NEW owner handles a retry —
// a bounded duplicate-send window across failover. shared_outbox fences every
// send by outbox version and closes that window, so it is required for this
// class. Non-clustered or non-HTTP-exclusive routes are unaffected. An empty
// delivery_mode defaults to direct_hold (domain/routing.RoutePolicy.WithDefaults),
// so empty and explicit direct_hold are the same hazardous class here.
func validateClusteredExclusiveHTTPDirectHold(ve *ValidationError, cfg *ports.BridgeConfig) {
	if !deploymentIsClustered(cfg) {
		return
	}
	for _, r := range cfg.Routes {
		if !routeIsExclusive(cfg, r) {
			continue
		}
		if receiverTransport(cfg, r.ReceiverID) != "http" {
			continue
		}
		if r.DeliveryMode != "" && r.DeliveryMode != string(routing.DeliveryDirectHold) {
			continue // shared_outbox (or any non-direct_hold mode) is fenced-safe
		}
		mode := r.DeliveryMode
		if mode == "" {
			mode = string(routing.DeliveryDirectHold) + " (default)"
		}
		ve.Addf("routes[%s]: clustered exclusive route with HTTP ingress uses %s delivery, which is "+
			"not fencing-safe across failover: a request forwarded to an owner that has just stepped "+
			"down can be sent by the old owner while the new owner handles a retry, duplicating the "+
			"send. Use delivery_mode: shared_outbox (fenced by outbox version) for this route",
			r.ID, mode)
	}
}

// receiverTransport returns the transport name of the receiver with the given ID
// (the route's ingress), or "" when no such receiver exists.
func receiverTransport(cfg *ports.BridgeConfig, receiverID string) string {
	for i := range cfg.Receivers {
		if cfg.Receivers[i].ID == receiverID {
			return cfg.Receivers[i].Transport
		}
	}
	return ""
}

// routeIsExclusive reports whether a route participates in lease-managed,
// single-owner (exclusive) ownership: it either carries an inline session block
// (a RouteSessionDef is always exclusive) or its receiver references a session
// declared session_mode: exclusive. Mirrors bridge.hasExclusiveSessions' notion
// of an exclusive session, scoped to one route.
func routeIsExclusive(cfg *ports.BridgeConfig, r ports.RouteDef) bool {
	if r.Session != nil {
		return true
	}
	// "exclusive" is domain/connectivity.SessionExclusive; the literal is
	// duplicated to keep the config validator from importing connectivity for a
	// single discriminator string (same style as the other duplicated literals
	// in this file).
	for i := range cfg.Receivers {
		if cfg.Receivers[i].ID != r.ReceiverID {
			continue
		}
		sid := cfg.Receivers[i].SessionID
		if sid == "" {
			return false
		}
		for j := range cfg.Sessions {
			if cfg.Sessions[j].ID == sid && cfg.Sessions[j].SessionMode == "exclusive" {
				return true
			}
		}
		return false
	}
	return false
}

// validateSessionRenewTiming enforces the renew/lease invariant for every
// route session that sets renew_interval explicitly:
//
//	(renewInterval + jitter/2 + renewCallTimeout) * maxRenewFails < leaseTTL
//
// If it does not hold, the owner can burn through all its tolerated renewal
// failures (plus worst-case jitter AND the time each renew store call may burn
// before it fails) before the lease-loss is detected — the lease can lapse and
// a standby take over while the old owner still believes it holds it, risking
// split-brain. The per-attempt renewCallTimeout term matters because renewLoop
// resets its timer AFTER the renew call returns, so a hung backend adds up to
// renew_call_timeout to every attempt (finding H2). This mirrors the invariant
// runtime/session Config.Validate enforces (renewWorstCaseSpan); duplicating it
// in the config layer fails a statically-rejectable blueprint before any
// runtime is built (contract C3). renew_interval left empty is derived
// downstream and is safe, so it is not checked here; lease_ttl empty falls back
// to the runtime default.
func validateSessionRenewTiming(ve *ValidationError, cfg *ports.BridgeConfig) {
	// Mirrors runtime/session.DefaultConfig defaults so the config-layer check
	// rejects exactly the set the runtime would (never more, never less). The
	// config package must not import runtime/session (layering), so the
	// literals are duplicated with this note.
	const (
		defaultMaxRenewFails = 3
		defaultLeaseTTL      = 360 * time.Second
	)
	for _, r := range cfg.Routes {
		s := r.Session
		if s == nil || s.RenewInterval == "" {
			continue
		}
		renew, err := time.ParseDuration(s.RenewInterval)
		if err != nil {
			ve.Addf("routes[%s].session.renew_interval: invalid duration %q: %v", r.ID, s.RenewInterval, err)
			continue
		}
		if renew <= 0 {
			continue
		}
		lease := defaultLeaseTTL
		if s.LeaseTTL != "" {
			lease, err = time.ParseDuration(s.LeaseTTL)
			if err != nil {
				ve.Addf("routes[%s].session.lease_ttl: invalid duration %q: %v", r.ID, s.LeaseTTL, err)
				continue
			}
		}
		var jitter time.Duration
		if s.RenewJitter != "" {
			jitter, err = time.ParseDuration(s.RenewJitter)
			if err != nil {
				ve.Addf("routes[%s].session.lease_renew_jitter: invalid duration %q: %v", r.ID, s.RenewJitter, err)
				continue
			}
		}
		maxFails := s.MaxRenewFails
		if maxFails <= 0 {
			maxFails = defaultMaxRenewFails
		}
		// renew_call_timeout is an operator-settable blueprint field (finding
		// H2 folded it into the safety invariant). When unset/zero the runtime
		// derives it as min(renewInterval/2, 5s) floored at 1s
		// (runtime/session.deriveRenewCallTimeout); the literals below duplicate
		// that derivation so the config-layer worst case matches the runtime's.
		callTimeout, err := parseRenewCallTimeout(s, renew)
		if err != nil {
			ve.Addf("routes[%s].session.renew_call_timeout: invalid duration %q: %v", r.ID, s.RenewCallTimeout, err)
			continue
		}
		worst := (renew + jitter/2 + callTimeout) * time.Duration(maxFails)
		if worst >= lease {
			ve.Addf("routes[%s].session: worst-case renew span %s "+
				"((renew_interval=%s + lease_renew_jitter/2=%s + renew_call_timeout=%s) * max_renew_fails=%d) must be < "+
				"lease_ttl (%s); otherwise the owner can exhaust all tolerated renewal failures before "+
				"the lease expires, risking split-brain takeover — lower renew_interval/max_renew_fails/renew_call_timeout "+
				"or raise lease_ttl",
				r.ID, worst, renew, jitter/2, callTimeout, maxFails, lease)
		}
	}
}

// parseRenewCallTimeout returns the per-attempt renew-call-timeout term used in
// the worst-case span. It parses the operator-set renew_call_timeout, or, when
// unset/zero, duplicates runtime/session.deriveRenewCallTimeout's fallback
// (min(renewInterval/2, 5s) floored at 1s) so the config-layer invariant folds
// in exactly the term the runtime uses. The config package must not import
// runtime/session (layering), so the literals are duplicated with this note.
func parseRenewCallTimeout(s *ports.RouteSessionDef, renewInterval time.Duration) (time.Duration, error) {
	const (
		maxDerivedTimeout = 5 * time.Second
		minDerivedTimeout = 1 * time.Second
	)
	if s.RenewCallTimeout != "" {
		ct, err := time.ParseDuration(s.RenewCallTimeout)
		if err != nil {
			return 0, fmt.Errorf("parse renew_call_timeout %q: %w", s.RenewCallTimeout, err)
		}
		if ct > 0 {
			return ct, nil
		}
	}
	t := renewInterval / 2
	if t > maxDerivedTimeout {
		t = maxDerivedTimeout
	}
	if t < minDerivedTimeout {
		t = minDerivedTimeout
	}
	return t, nil
}

// validateConnectLeaseBudget warns when a deferred-connect (connect_after_lease)
// lease-bound session's broker connect+reconcile budget can consume so much of
// the lease TTL that the FIRST renewal completes at or after expiry (finding F2).
// For such a session the lease is acquired FIRST, then the broker connect AND the
// subscription reconcile run (each bounded by the transport's connect budget),
// and only then does the renew loop start — so the first renewal completes no
// earlier than
//
//	connect_timeout + reconcile + renewInterval + jitter/2 + renewCallTimeout
//
// after acquisition. The reconcile term is included because the runtime does not
// begin renewing until subscriptions are re-established (ports.SessionReconciled);
// omitting it understates real failover time (finding H4 — the budget math must
// count connect AND reconcile, not connect alone). There is no dedicated
// reconcile-timeout knob, so the subscribe round-trip is modelled conservatively
// as one additional connect_timeout worth of broker interaction — an advisory
// over-estimate is the SAFE direction here (understating failover is the bug).
// If that first renewal meets or exceeds lease_ttl the lease has already lapsed
// before it lands: the owner definitively loses the lease it just won, steps
// down, and (for a single-use MQTT session) restarts, producing a cluster-wide
// restart loop under broker slowness.
//
// This is an advisory WARNING, not a hard reject, because:
//   - connect_timeout is a transport-specific raw option (mqtt/amqp) read
//     defensively from the session raw map; it is only inspected when the
//     operator set it EXPLICITLY. The config layer cannot know a transport's
//     DEFAULT connect budget (e.g. paho defaults to 30s) without baking
//     transport specifics into a transport-neutral validator, so a large
//     default connect_timeout paired with a small lease_ttl is NOT caught here
//     — that residual gap is noted for the docs pass.
//   - the reconcile/subscribe budget after connect has no dedicated knob, so it
//     is conservatively approximated (see above) rather than measured.
//
// Eager-connect sessions (connect_after_lease=false) connect BEFORE acquiring
// the lease, so their connect budget does not erode the post-acquire TTL and is
// skipped. nil defaults to true for these always-exclusive sessions (finding F6).
func validateConnectLeaseBudget(ve *ValidationError, cfg *ports.BridgeConfig) {
	const (
		defaultMaxRenewFails = 3
		defaultLeaseTTL      = 360 * time.Second
	)
	for _, r := range cfg.Routes {
		s := r.Session
		if s == nil {
			continue
		}
		if s.ConnectAfterLease != nil && !*s.ConnectAfterLease {
			continue // eager connect happens before acquire; TTL not eroded
		}
		connect, ok := sessionConnectTimeout(cfg, s.SessionID)
		if !ok {
			continue // connect_timeout not explicitly set / not a duration
		}
		lease := defaultLeaseTTL
		if s.LeaseTTL != "" {
			d, err := time.ParseDuration(s.LeaseTTL)
			if err != nil {
				continue // invalid duration is rejected on the main validation path; skip the advisory rather than double-report
			}
			lease = d
		}
		maxFails := s.MaxRenewFails
		if maxFails <= 0 {
			maxFails = defaultMaxRenewFails
		}
		var renew time.Duration
		if s.RenewInterval != "" {
			d, err := time.ParseDuration(s.RenewInterval)
			if err != nil || d <= 0 {
				continue
			}
			renew = d
		} else {
			renew = derivedRenewIntervalForConfig(lease, maxFails)
		}
		jitter := renew / 4 // deriveRenewJitter default (duplicated, see note below)
		if s.RenewJitter != "" {
			if d, err := time.ParseDuration(s.RenewJitter); err == nil {
				jitter = d
			}
		}
		callTimeout, err := parseRenewCallTimeout(s, renew)
		if err != nil {
			continue
		}
		// reconcile: the renew loop does not start until subscriptions are
		// re-established after connect (ports.SessionReconciled). No dedicated
		// reconcile-timeout knob exists, so model the subscribe round-trip
		// conservatively as one more connect_timeout worth of broker
		// interaction. Including it stops the failover budget from understating
		// real failover time (finding H4).
		reconcile := connect
		firstRenew := connect + reconcile + renew + jitter/2 + callTimeout
		if firstRenew >= lease {
			ve.Warnf("routes[%s].session: connect + reconcile + first-renew span %s "+
				"(connect_timeout=%s + reconcile≈connect_timeout=%s + renew_interval=%s + lease_renew_jitter/2=%s + renew_call_timeout=%s) "+
				"is >= lease_ttl (%s) for a connect_after_lease session; the broker connect AND subscription reconcile run AFTER "+
				"the lease is won, so the first renewal can land at/after expiry, definitively losing the just-won lease and "+
				"(for single-use MQTT) restarting the owner — raise lease_ttl or lower connect_timeout/renew_interval",
				r.ID, firstRenew, connect, reconcile, renew, jitter/2, callTimeout, lease)
		}
	}
}

// sessionConnectTimeout returns the EXPLICITLY configured connect_timeout for
// the session with the given ID, if present and expressible as a duration.
// connect_timeout is a transport-specific raw option (mqtt/amqp), so it is read
// defensively from the session raw map (mirroring validateStaleClaimDuration); a
// missing key, an unknown/absent raw map, or a non-duration value returns
// ok=false so the F2 advisory is simply skipped rather than emitting a spurious
// error the transport validator owns.
func sessionConnectTimeout(cfg *ports.BridgeConfig, sessionID string) (time.Duration, bool) {
	for i := range cfg.Sessions {
		if cfg.Sessions[i].ID != sessionID {
			continue
		}
		raw, ok := rawMap(cfg.Sessions[i].Raw())["connect_timeout"]
		if !ok {
			return 0, false
		}
		switch v := raw.(type) {
		case string:
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return 0, false
			}
			return d, true
		case time.Duration:
			if v <= 0 {
				return 0, false
			}
			return v, true
		default:
			return 0, false
		}
	}
	return 0, false
}

// derivedRenewIntervalForConfig duplicates runtime/session.deriveRenewInterval so
// the F2 advisory uses the SAME effective renew interval the runtime derives when
// renew_interval is left empty. The config package must not import runtime/session
// (layering), so the formula and its literals are duplicated with this note; keep
// it in sync with deriveRenewInterval/deriveRenewJitter.
func derivedRenewIntervalForConfig(ttl time.Duration, maxRenewFails int) time.Duration {
	if maxRenewFails < 1 {
		maxRenewFails = 1
	}
	perAttempt := (ttl * 3) / (time.Duration(maxRenewFails) * 4)
	reserve := 5 * time.Second
	if reserve > perAttempt/2 {
		reserve = perAttempt / 2
	}
	interval := ((perAttempt - reserve) * 8) / 9
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	return interval
}

func validateBridgeFields(ve *ValidationError, cfg *ports.BridgeConfig) {
	if cfg.Bridge.ID == "" {
		ve.Add("bridge.id is required")
	}

	if cfg.Bridge.DeploymentMode != "" {
		validateEnum(ve, "bridge.deployment_mode", cfg.Bridge.DeploymentMode,
			"standalone", "clustered")
	}

	if cfg.Bridge.ShutdownTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.ShutdownTimeout); err != nil {
			ve.Addf("bridge.shutdown_timeout: invalid duration %q: %v", cfg.Bridge.ShutdownTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.shutdown_timeout: must be positive, got %s", cfg.Bridge.ShutdownTimeout)
		}
	}
	if cfg.Bridge.DrainTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.DrainTimeout); err != nil {
			ve.Addf("bridge.drain_timeout: invalid duration %q: %v", cfg.Bridge.DrainTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.drain_timeout: must be positive, got %s", cfg.Bridge.DrainTimeout)
		}
	}
	var perRecordDrain, maxDrain time.Duration
	if cfg.Bridge.PerRecordDrainTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.PerRecordDrainTimeout); err != nil {
			ve.Addf("bridge.per_record_drain_timeout: invalid duration %q: %v", cfg.Bridge.PerRecordDrainTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.per_record_drain_timeout: must be positive, got %s", cfg.Bridge.PerRecordDrainTimeout)
		} else {
			perRecordDrain = d
		}
	}
	if cfg.Bridge.MaxDrainTimeout != "" {
		if d, err := time.ParseDuration(cfg.Bridge.MaxDrainTimeout); err != nil {
			ve.Addf("bridge.max_drain_timeout: invalid duration %q: %v", cfg.Bridge.MaxDrainTimeout, err)
		} else if d <= 0 {
			ve.Addf("bridge.max_drain_timeout: must be positive, got %s", cfg.Bridge.MaxDrainTimeout)
		} else {
			maxDrain = d
		}
	}
	if perRecordDrain > 0 && maxDrain > 0 && maxDrain < perRecordDrain {
		ve.Addf("bridge.max_drain_timeout (%s) must be >= per_record_drain_timeout (%s)",
			cfg.Bridge.MaxDrainTimeout, cfg.Bridge.PerRecordDrainTimeout)
	}
}

func validateConfigWatchFields(ve *ValidationError, cfg *ports.BridgeConfig) {
	cw := cfg.ConfigWatch
	if cw == nil {
		return
	}
	if cw.Mode != "" {
		validateEnum(ve, "config_watch.mode", cw.Mode, "notify", "poll")
	}
	if cw.PollInterval != "" {
		if d, err := time.ParseDuration(cw.PollInterval); err != nil {
			ve.Addf("config_watch.poll_interval: invalid duration %q: %v", cw.PollInterval, err)
		} else if d <= 0 {
			ve.Addf("config_watch.poll_interval: must be positive, got %s", cw.PollInterval)
		}
	}
	if cw.Debounce != "" {
		if d, err := time.ParseDuration(cw.Debounce); err != nil {
			ve.Addf("config_watch.debounce: invalid duration %q: %v", cw.Debounce, err)
		} else if d <= 0 {
			ve.Addf("config_watch.debounce: must be positive, got %s", cw.Debounce)
		}
	}
}

func validateEnum(ve *ValidationError, field, value string, allowed ...string) {
	for _, a := range allowed {
		if value == a {
			return
		}
	}
	ve.Addf("%s: invalid value %q, must be one of: %s", field, value, strings.Join(allowed, ", "))
}

// validateStaleClaimDuration warns when the outbox store's
// stale_claim_duration is explicitly set to a value much larger than the
// session step-down grace periods. The real lower bound on
// stale_claim_duration is the worst-case drain-batch timeout: a stranded
// claim must exceed it before the SAME owner can reclaim it (this recovery
// path is DynamoDB-outbox-only). A value far above that ceiling only delays
// same-owner stranded-claim recovery — it does not gate failover hand-off,
// which a new owner drives immediately via its strictly higher fencing
// version. The 2x-max-StepDownGrace threshold is a cheap proxy for "much
// larger than the drain ceiling"; it does not change the recovery semantics.
func validateStaleClaimDuration(ve *ValidationError, cfg *ports.BridgeConfig) {
	if cfg.Stores.Outbox == nil {
		return
	}
	opts := rawMap(cfg.Stores.Outbox.Raw())
	if opts == nil {
		return
	}
	raw, ok := opts["stale_claim_duration"]
	if !ok {
		return
	}

	var stale time.Duration
	switch v := raw.(type) {
	case string:
		d, err := time.ParseDuration(v)
		if err != nil {
			ve.Addf("stores.outbox.options.stale_claim_duration: invalid duration %q: %v", v, err)
			return
		}
		stale = d
	case time.Duration:
		stale = v
	default:
		ve.Addf("stores.outbox.options.stale_claim_duration: must be a duration string (e.g. \"30s\"), got %T", raw)
		return
	}

	if stale <= 0 {
		ve.Addf("stores.outbox.options.stale_claim_duration: must be positive, got %v", stale)
		return
	}

	maxGrace := routing.DefaultStepDownGrace
	for _, r := range cfg.Routes {
		if r.Session == nil {
			continue
		}
		grace := routing.DefaultStepDownGrace
		if r.Session.StepDownGrace != "" {
			if d, err := time.ParseDuration(r.Session.StepDownGrace); err == nil {
				grace = d
			}
		}
		if grace > maxGrace {
			maxGrace = grace
		}
	}

	if stale > 2*maxGrace {
		ve.Warnf("stores.outbox.options.stale_claim_duration (%s) is more than 2x "+
			"the maximum step_down_grace (%s); a value this large only delays "+
			"same-owner stranded-claim recovery (DynamoDB outbox only) and does "+
			"not gate failover hand-off. The real lower bound is the worst-case "+
			"drain-batch timeout, which a stranded claim must exceed before it "+
			"can be reclaimed; step_down_grace + 15s (%s) is only a rule-of-thumb "+
			"starting point that usually clears that ceiling, not the constraint",
			stale, maxGrace, maxGrace+15*time.Second)
	}
}
