package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ─────────────────────────────────────────────────────────────────────────
// Finding 1 — auth throttle must not lock out valid credentials, and the
// admin/monitor throttle scopes must be independent.
// ─────────────────────────────────────────────────────────────────────────

// TestAuthThrottle_MonitorScopeDoesNotLockAdminScope pins the scope split: a
// peer that saturates the MONITOR throttle with bad keys must NOT throttle the
// ADMIN plane from the same peer — a monitor-plane attacker cannot lock out the
// admin API, and a valid admin key still passes.
func TestAuthThrottle_MonitorScopeDoesNotLockAdminScope(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	rt := runtime.New(runtime.WithInstanceID("throttle-scope"))
	cfg := testConfig()
	cfg.AuthFailureLimit = 3
	cfg.AuthFailureWindow = time.Minute
	s := New(rt, cfg, WithClock(clk))

	okAdmin := s.requireAdminAuth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	okMonitor := s.requireMonitorAuth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	const peer = "10.0.0.7:5555"
	do := func(h http.HandlerFunc, path, key string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-API-Key", key)
		req.RemoteAddr = peer
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}

	// Saturate the MONITOR throttle for this peer with bad keys.
	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusUnauthorized, do(okMonitor, "/api/v1/monitor/topology", "wrong"))
	}
	require.Equal(t, http.StatusTooManyRequests, do(okMonitor, "/api/v1/monitor/topology", "wrong"),
		"the monitor plane must throttle its own scope after the limit")

	// The ADMIN plane from the SAME peer is a SEPARATE throttle scope: a wrong
	// admin key still gets a normal 401 (freshly scored), not a 429 leaked from
	// the monitor plane.
	assert.Equal(t, http.StatusUnauthorized, do(okAdmin, "/api/v1/admin/bridge", "wrong"),
		"monitor-plane throttling must not leak into the admin plane")

	// And a VALID admin key passes even while the monitor scope is throttled.
	assert.Equal(t, http.StatusOK, do(okAdmin, "/api/v1/admin/bridge", "test-secret-key-0123456789"))
}

// ─────────────────────────────────────────────────────────────────────────
// Finding 2 — config-txn GET must not panic when the txn expires concurrently.
// ─────────────────────────────────────────────────────────────────────────

func newConfigTxnServer(t *testing.T, store ports.ConfigStore, clk *clocktest.Fake) *Server {
	t.Helper()
	rt := runtime.New(runtime.WithInstanceID("cfgtxn-get"))
	cfg := testConfig()
	cfg.ConfigStore = store
	cfg.ConfigProvider = func() *ports.BridgeConfig { return sampleBridgeConfig() }
	// Single-process test server: assert single-writer so a durable commit
	// against a non-CAS test store is not fail-closed refused (see [HIGH-1]).
	cfg.ConfigSingleWriter = true
	s := New(rt, cfg, WithClock(clk))
	require.NotNil(t, s.configTxn, "config txn manager must be wired when store+provider are set")
	return s
}

func configTxnGet(s *Server, txnID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/config/txn/"+txnID, nil)
	req.SetPathValue("txnID", txnID)
	rec := httptest.NewRecorder()
	s.handleConfigTxnGet(rec, req)
	return rec
}

// TestHandleConfigTxnGet_ExpiredReturns404NoPanic pins finding 2: an active GET
// returns 200 with the txn metadata from ONE locked snapshot, and a GET after
// the TTL elapses returns a clean 404 instead of dereferencing a nil
// transaction (the pre-fix Preview()+Active() split raced expiry to nil here).
func TestHandleConfigTxnGet_ExpiredReturns404NoPanic(t *testing.T) {
	disk := sampleBridgeConfig()
	disk.Version = 1
	store := &guardTestStore{disk: disk}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	s := newConfigTxnServer(t, store, clk)

	txn, err := s.configTxn.Begin(context.Background(), time.Minute)
	require.NoError(t, err)

	// Active GET returns 200 with the txn metadata rendered from the same
	// locked snapshot as the preview.
	rec := configTxnGet(s, txn.ID)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, txn.ID, body["txn_id"])

	// Advance past the TTL: the next GET must 404 (checkTxn expires it), never
	// panic on a nil transaction.
	clk.Advance(time.Minute + time.Second)
	rec2 := configTxnGet(s, txn.ID)
	assert.Equal(t, http.StatusNotFound, rec2.Code, "body=%s", rec2.Body.String())
}

// TestHandleConfigTxnGet_ConcurrentTeardownNeverPanics runs concurrent GETs
// against a txn ID while another goroutine repeatedly rolls back and re-begins,
// racing the read against teardown. The atomic Preview must never yield a 500
// (a recovered nil-deref) — every response is a clean 200 or 404. Run under
// -race to also prove the manager locking is sound.
func TestHandleConfigTxnGet_ConcurrentTeardownNeverPanics(t *testing.T) {
	disk := sampleBridgeConfig()
	disk.Version = 1
	store := &guardTestStore{disk: disk}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	s := newConfigTxnServer(t, store, clk)

	txn, err := s.configTxn.Begin(context.Background(), time.Minute)
	require.NoError(t, err)

	const readers = 6
	done := make(chan struct{})
	bad := make(chan int, readers)

	for i := 0; i < readers; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				code := configTxnGet(s, txn.ID).Code
				if code != http.StatusOK && code != http.StatusNotFound {
					bad <- code
					return
				}
			}
		}()
	}

	// Churn teardown/re-begin on the manager to race the readers.
	for i := 0; i < 300; i++ {
		_ = s.configTxn.Rollback(txn.ID)
		_, _ = s.configTxn.Begin(context.Background(), time.Minute)
	}
	close(done)

	select {
	case code := <-bad:
		t.Fatalf("concurrent GET during teardown returned %d; want only 200 or 404 (no panic/500)", code)
	default:
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Finding 3 — Commit must apply on a DETACHED context outside the manager lock.
// ─────────────────────────────────────────────────────────────────────────

// TestConfigTxnCommit_ApplyDetachedFromRequestContext pins finding 3: the
// durable save happens under the lock, then the applier runs on a context that
// is NOT cancelled by a client disconnect and NOT under the manager lock. The
// test cancels the request context mid-apply and asserts (a) the applier's
// context is not cancelled, (b) another manager op (Begin) is not blocked while
// the apply is in flight, and (c) Commit reports success once the apply returns.
func TestConfigTxnCommit_ApplyDetachedFromRequestContext(t *testing.T) {
	disk := sampleBridgeConfig()
	disk.Version = 1
	store := &guardTestStore{disk: disk}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	applyStarted := make(chan struct{})
	releaseApply := make(chan struct{})
	applyCtxErr := make(chan error, 1)
	applier := func(ctx context.Context, _ *ports.BridgeConfig) error {
		close(applyStarted)
		<-releaseApply
		applyCtxErr <- ctx.Err()
		return nil
	}

	mgr := newTxnManager(store, sampleBridgeConfig, applier, nil, clk)
	txn, err := mgr.Begin(context.Background(), time.Minute)
	require.NoError(t, err)

	reqCtx, cancelReq := context.WithCancel(context.Background())
	type commitResult struct {
		version int
		err     error
	}
	commitDone := make(chan commitResult, 1)
	go func() {
		v, err := mgr.Commit(reqCtx, txn.ID)
		commitDone <- commitResult{version: v, err: err}
	}()

	// The apply has started, which means commitDurable already saved and
	// released the manager lock.
	wait.RequireClosed(t, applyStarted, 2*time.Second)

	// Client disconnect mid-apply: cancelling the request context must NOT reach
	// the detached apply context.
	cancelReq()

	// The manager lock is released during the apply, so a fresh Begin must not
	// block behind the in-flight (still-blocked) apply.
	beginDone := make(chan error, 1)
	go func() {
		_, err := mgr.Begin(context.Background(), time.Minute)
		beginDone <- err
	}()
	require.NoError(t, wait.RequireReceive(t, beginDone, 2*time.Second),
		"a new txn must be able to begin while the detached apply runs")

	// Release the apply and collect results.
	close(releaseApply)
	assert.NoError(t, wait.RequireReceive(t, applyCtxErr, 2*time.Second),
		"the applier must run on a context detached from the cancelled request context")

	res := wait.RequireReceive(t, commitDone, 2*time.Second)
	require.NoError(t, res.err, "apply succeeded, so Commit must report success despite the request-ctx cancellation")
	assert.Equal(t, 2, res.version, "version must increment from disk v1 to v2")
	assert.Equal(t, 1, store.saveCalls, "the durable Save must have happened before the apply")
}

// TestConfigTxnCommit_ApplyFailureRollsBack pins the restart-bomb fix: a failed
// in-band apply restores the previous on-disk config (errConfigRolledBack) and
// reports the RESTORED version, instead of leaving the rejected config on disk
// to crash-loop the next boot (the old committed_not_applied behaviour).
func TestConfigTxnCommit_ApplyFailureRollsBack(t *testing.T) {
	disk := sampleBridgeConfig()
	disk.Version = 1
	store := &guardTestStore{disk: disk}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	applyErr := errors.New("runtime did not converge")
	mgr := newTxnManager(store, sampleBridgeConfig, func(context.Context, *ports.BridgeConfig) error {
		return applyErr
	}, nil, clk)

	txn, err := mgr.Begin(context.Background(), time.Minute)
	require.NoError(t, err)

	version, err := mgr.Commit(context.Background(), txn.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, errConfigRolledBack)
	assert.ErrorIs(t, err, applyErr)
	assert.NotErrorIs(t, err, errConfigApplyFailed, "a successful rollback must not report committed_not_applied")
	assert.Equal(t, 1, version, "rolled_back must report the RESTORED (previous) version, not the rejected one")
	assert.Equal(t, 2, store.saveCalls, "one Save for the durable commit, one for the rollback restore")
}

// ─────────────────────────────────────────────────────────────────────────
// Finding 4a — redrive without redrive-safe injection must not report a bare
// success; it must surface the dedup-swallow hazard.
// ─────────────────────────────────────────────────────────────────────────

// TestHandleDLQRedrive_WarnsWhenNoRedriveSafeInjection pins finding 4a: a
// runtime lacking InjectRedrive replays a COLLISION-FREE direct entry (empty
// envelope id, no dedup key) via plain Inject; the response must still carry a
// warning so a 200 does not hide the possible no-op on any deduped path. (An
// ID-bearing or dedup-keyed entry is refused instead — see
// TestHandleDLQRedrive_NoRedriveSafe_DirectEntryWithID_RefusedNotDeleted.)
func TestHandleDLQRedrive_WarnsWhenNoRedriveSafeInjection(t *testing.T) {
	mux, dlq, _ := newRecordingRedriveServer(t)
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "r1",
		Envelope: messaging.Envelope{},
		FailedAt: time.Now(),
	}))

	out := httptest.NewRecorder()
	mux.ServeHTTP(out, redriveReq(`{"ids":["e1"]}`))
	require.Equal(t, http.StatusOK, out.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &body))
	require.Equal(t, float64(1), body["redriven"])
	warning, _ := body["warning"].(string)
	assert.NotEmpty(t, warning, "a runtime without redrive-safe injection must surface a dedup-swallow warning")
	assert.Contains(t, warning, "dedup")
}

// TestHandleDLQRedrive_NoWarningWhenRedriveSafe is the negative control: a real
// runtime implements InjectRedrive, so no dedup warning is emitted.
func TestHandleDLQRedrive_NoWarningWhenRedriveSafe(t *testing.T) {
	mux, dlq, _ := redriveSetup(t)
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "test-route",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
		FailedAt: time.Now(),
	}))

	out := httptest.NewRecorder()
	mux.ServeHTTP(out, redriveReq(`{"ids":["e1"]}`))
	require.Equal(t, http.StatusOK, out.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &body))
	require.Equal(t, float64(1), body["redriven"])
	assert.Nil(t, body["warning"], "a redrive-safe runtime must not emit a dedup warning")
}

// ─────────────────────────────────────────────────────────────────────────
// Finding 5 — redrive must emit a begin audit record BEFORE the first claim.
// ─────────────────────────────────────────────────────────────────────────

// TestHandleDLQRedrive_EmitsBeginAuditBeforeOutcome pins finding 5: a
// dlq.redrive.begin record carrying the entry IDs is emitted BEFORE the outcome
// record, so a crash between Delete and Inject leaves an audit trace of which
// entries were in flight.
func TestHandleDLQRedrive_EmitsBeginAuditBeforeOutcome(t *testing.T) {
	mux, dlq, audit := newAuditedRedriveServer(t)
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "test-route",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
		FailedAt: time.Now(),
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1"]}`))
	require.Equal(t, http.StatusOK, rec.Code)

	events := audit.Events()
	beginIdx, outcomeIdx := -1, -1
	for i, ev := range events {
		switch ev.Action {
		case "dlq.redrive.begin":
			beginIdx = i
		case "dlq.redrive":
			outcomeIdx = i
		}
	}
	require.GreaterOrEqual(t, beginIdx, 0, "a dlq.redrive.begin record must be emitted")
	require.GreaterOrEqual(t, outcomeIdx, 0, "a dlq.redrive outcome record must be emitted")
	assert.Less(t, beginIdx, outcomeIdx, "the begin record must precede the outcome record")
	assert.Equal(t, "pending", events[beginIdx].Outcome)
	assert.Equal(t, []string{"e1"}, events[beginIdx].Detail["ids"])
}

// newAuditedRedriveServer mirrors redriveSetup but records audit events so the
// begin/outcome ordering can be observed.
func newAuditedRedriveServer(t *testing.T) (*http.ServeMux, *memorydlq.Store, *recordingAuditLogger) {
	t.Helper()
	dlq := memorydlq.NewStore()
	sender := &stubSender{}
	recv := newStubReceiver()
	rt := runtime.New(
		runtime.WithInstanceID("redrive-audit"),
		runtime.WithDLQStore(dlq),
	)
	cfg := runtime.RouteConfig{
		ID:                 "test-route",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	require.NoError(t, rt.AddRoute(cfg, recv, sender, nil, nil))
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	<-recv.ready // wait for the receiver goroutine instead of sleeping

	audit := &recordingAuditLogger{}
	s := New(rt, testConfig(), WithAuditLogger(audit))
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)
	return mux, dlq, audit
}

// ─────────────────────────────────────────────────────────────────────────
// Finding 4b — inject with a caller-supplied envelope id must surface the
// dedup-swallow hazard.
// ─────────────────────────────────────────────────────────────────────────

// TestHandleInject_CallerSuppliedID_SurfacesDedupWarning pins finding 4b: a
// caller-supplied envelope id can collide with a completed/poisoned outbox row
// on a shared_outbox route and be silently swallowed, so the response carries a
// warning and the audit detail flags caller_supplied_id.
func TestHandleInject_CallerSuppliedID_SurfacesDedupWarning(t *testing.T) {
	s, _, audit := newInjectTestServer(t)

	rec := httptest.NewRecorder()
	s.handleInject(rec, newInjectReq(`{"id":"caller-1","subject":"s"}`))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "caller-1", resp["envelope_id"])
	assert.NotEmpty(t, resp["warning"], "a caller-supplied id must surface a dedup-swallow warning")

	assert.Equal(t, true, audit.last().Detail["caller_supplied_id"],
		"the audit detail must flag a caller-supplied id")
}

// TestHandleInject_GeneratedID_NoDedupWarning is the negative control: a
// server-generated id is unique, so no dedup warning or flag is emitted.
func TestHandleInject_GeneratedID_NoDedupWarning(t *testing.T) {
	s, _, audit := newInjectTestServer(t)

	rec := httptest.NewRecorder()
	s.handleInject(rec, newInjectReq(`{"subject":"s"}`))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, hasWarning := resp["warning"]
	assert.False(t, hasWarning, "a server-generated id must not carry a dedup warning")

	_, flagged := audit.last().Detail["caller_supplied_id"]
	assert.False(t, flagged, "a server-generated id must not be flagged caller_supplied_id")
}

// ─────────────────────────────────────────────────────────────────────────
// Finding 7b — handleStart maps a controller-path failure to 500, not 409.
// ─────────────────────────────────────────────────────────────────────────

// TestHandleStart_ControllerFailure_Returns500NotConflict pins finding 7b: a
// BridgeController StartBridge error is a genuine build/start failure (409 is
// reserved for already-running), and the internal error text is not leaked.
func TestHandleStart_ControllerFailure_Returns500NotConflict(t *testing.T) {
	ctrl := &fakeBridgeController{startErr: errors.New("INTERNAL_BUILD_SECRET")}
	rt := runtime.New(runtime.WithInstanceID("start-fail"))
	cfg := testConfig()
	cfg.BridgeController = ctrl
	s := New(rt, cfg)

	rec := httptest.NewRecorder()
	s.handleStart(rec, adminRequest(http.MethodPost, "/api/v1/admin/bridge/start"))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "INTERNAL_BUILD_SECRET",
		"internal error text must not leak to the client")
	assert.Equal(t, 1, ctrl.startCalls)
}

// ─────────────────────────────────────────────────────────────────────────
// Finding 7d — /ready nil-runtime branch must set Cache-Control.
// ─────────────────────────────────────────────────────────────────────────

// TestHandleReady_NilRuntime_SetsCacheControl pins finding 7d: the nil-runtime
// 503 must carry the no-cache header too, so a cached "not ready" (or a stale
// "ready") never lingers at an intermediary.
func TestHandleReady_NilRuntime_SetsCacheControl(t *testing.T) {
	cfg := testConfig()
	cfg.RuntimeProvider = func() ports.Runtime { return nil }
	s := New(nil, cfg)

	rec := httptest.NewRecorder()
	s.handleReady(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor/ready", nil))

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "no-cache, max-age=0", rec.Header().Get("Cache-Control"),
		"the nil-runtime readiness response must forbid caching")
}

// ─────────────────────────────────────────────────────────────────────────
// Finding 7e — ValidateMonitorKey mirrors the admin key length floor.
// ─────────────────────────────────────────────────────────────────────────

// TestValidateMonitorKey pins finding 7e: the monitor key is validated against
// the same minimum length as the admin key (empty is allowed — monitor auth is
// optional), so a reload/bootstrap cannot install a too-short monitor key.
func TestValidateMonitorKey(t *testing.T) {
	assert.NoError(t, ValidateMonitorKey(""), "an empty monitor key is allowed (monitor auth optional)")
	assert.NoError(t, ValidateMonitorKey("0123456789abcdef"), "a 16-char key meets the floor")

	err := ValidateMonitorKey("short")
	require.Error(t, err, "a below-floor monitor key must be rejected")
	assert.NotContains(t, err.Error(), "short", "the error must not echo the key material")
}

// ─────────────────────────────────────────────────────────────────────────
// Finding 8 — DLQ redrive outcomes must be metrically visible (route-tagged).
// ─────────────────────────────────────────────────────────────────────────

// recordingMetrics is a ports.MetricsExporter that records Counter calls so a
// test can assert redrive metrics fire with the expected route_id tag.
type recordingMetrics struct {
	mu       sync.Mutex
	counters []counterCall
}

type counterCall struct {
	name  string
	value int64
	tags  []shared.Tag
}

func (m *recordingMetrics) Counter(name string, value int64, tags ...shared.Tag) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters = append(m.counters, counterCall{
		name:  name,
		value: value,
		tags:  append([]shared.Tag(nil), tags...),
	})
}

func (m *recordingMetrics) Gauge(string, float64, ...shared.Tag)       {}
func (m *recordingMetrics) Histogram(string, float64, ...shared.Tag)   {}
func (m *recordingMetrics) Timer(string, time.Duration, ...shared.Tag) {}
func (m *recordingMetrics) Flush(context.Context) error                { return nil }
func (m *recordingMetrics) Close(context.Context) error                { return nil }

var _ ports.MetricsExporter = (*recordingMetrics)(nil)

// countFor sums the Counter values emitted under name that carry a route_id tag
// equal to routeID.
func (m *recordingMetrics) countFor(name, routeID string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	for _, c := range m.counters {
		if c.name != name {
			continue
		}
		for _, tag := range c.tags {
			if tag.Key == shared.TagKeyRouteID && tag.Value == routeID {
				total += c.value
			}
		}
	}
	return total
}

// newRedriveServerWithMetrics builds a redrive-capable server whose single
// route is "test-route", wired to the given metrics exporter.
func newRedriveServerWithMetrics(t *testing.T, metrics ports.MetricsExporter) (*http.ServeMux, *memorydlq.Store) {
	t.Helper()
	dlq := memorydlq.NewStore()
	sender := &stubSender{}
	recv := newStubReceiver()
	rt := runtime.New(
		runtime.WithInstanceID("redrive-metrics"),
		runtime.WithDLQStore(dlq),
	)
	cfg := runtime.RouteConfig{
		ID:                 "test-route",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	require.NoError(t, rt.AddRoute(cfg, recv, sender, nil, nil))
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	<-recv.ready // wait for the receiver goroutine instead of sleeping

	s := New(rt, testConfig(), WithMetrics(metrics))
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)
	return mux, dlq
}

// TestHandleDLQRedrive_EmitsRouteTaggedMetrics pins finding 8: a redrive emits
// DLQRedrives for each entry successfully redriven and DLQRedriveFailures for a
// claim-ok-but-inject-failed entry, each tagged with the entry's route_id, so
// manual-recovery outcomes are visible to alerting. The batch mixes one entry
// on the live route (success) with one on an absent route (inject fails after
// the claim, restore attempted) to exercise both counters in one request.
func TestHandleDLQRedrive_EmitsRouteTaggedMetrics(t *testing.T) {
	metrics := &recordingMetrics{}
	mux, dlq := newRedriveServerWithMetrics(t, metrics)

	seedDLQ(t, dlq,
		routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: "ok-1", RouteID: "test-route",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
			FailedAt: time.Now(),
		}),
		routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: "bad-1", RouteID: "missing-route",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s2"}),
			FailedAt: time.Now(),
		}),
	)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["ok-1","bad-1"]}`))
	require.Equal(t, http.StatusMultiStatus, rec.Code, "one entry failed → 207; body=%s", rec.Body.String())

	assert.Equal(t, int64(1), metrics.countFor(shared.MetricDLQRedrives, "test-route"),
		"a successful redrive must emit DLQRedrives tagged with its route_id")
	assert.Equal(t, int64(1), metrics.countFor(shared.MetricDLQRedriveFailures, "missing-route"),
		"a claim-ok-but-inject-failed redrive must emit DLQRedriveFailures tagged with its route_id")

	// The counters must not cross-tag: no failure on the live route, no success
	// on the absent one.
	assert.Equal(t, int64(0), metrics.countFor(shared.MetricDLQRedriveFailures, "test-route"))
	assert.Equal(t, int64(0), metrics.countFor(shared.MetricDLQRedrives, "missing-route"))
}

// TestHandleDLQRedrive_NoMetricsExporter_NoPanic pins the nil-safe contract: a
// server constructed WITHOUT WithMetrics (the default) redrives normally and
// never dereferences a nil exporter.
func TestHandleDLQRedrive_NoMetricsExporter_NoPanic(t *testing.T) {
	mux, dlq, _ := redriveSetup(t) // no WithMetrics
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "test-route",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
		FailedAt: time.Now(),
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1"]}`))
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(1), body["redriven"])
}
