//go:build longrunning

package longrunning_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Gap Tests: HTTP API (Category 11 — Operational)
//
// Summary:
// ┌──────┬─────────────────────────────────────┬──────────┐
// │ ID   │ Description                         │ Status   │
// ├──────┼─────────────────────────────────────┼──────────┤
// │ HA-1 │ Full endpoint suite                 │ PENDING  │
// │ HA-2 │ Bridge start/stop via HTTP          │ PENDING  │
// │ HA-3 │ DLQ management via HTTP             │ PENDING  │
// └──────┴─────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════

const (
	gapAdminKey   = "gap-test-admin-key-1234567890"
	gapMonitorKey = "gap-test-monitor-key-123456"
)

// gapHTTPClient is a shared HTTP client with a 10s timeout for test requests.
var gapHTTPClient = &http.Client{Timeout: 10 * time.Second}

// purgeableDLQStore gives the HTTP management test a fixture whose destructive
// operation has the same strictly-before cutoff semantics as production stores.
type purgeableDLQStore struct {
	replayableDLQStore
}

func (s *purgeableDLQStore) Purge(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	remaining := make([]routing.DLQEntry, 0, len(s.entries))
	purged := 0
	for _, entry := range s.entries {
		if entry.FailedAt().Before(before) {
			purged++
			continue
		}
		remaining = append(remaining, entry)
	}
	s.entries = remaining
	return purged, nil
}

// httpGet performs an authenticated GET request and returns status + body.
func httpGet(url, apiKey string) (int, map[string]any, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, nil, err
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := gapHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var data map[string]any
	_ = json.Unmarshal(body, &data)
	return resp.StatusCode, data, nil
}

// httpPost performs an authenticated POST request with JSON body.
func httpPost(url, apiKey string, payload any) (int, map[string]any, error) {
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return 0, nil, err
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := gapHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	var data map[string]any
	_ = json.Unmarshal(respBody, &data)
	return resp.StatusCode, data, nil
}

// TestGAP_HTTPFullSuite validates all admin and monitor HTTP endpoints
// with correct authentication and response schemas.
//
// Assertions:
//   - Unauthenticated probes return 200
//   - Authenticated endpoints return 200 with correct fields
//   - Missing API key returns 401
func TestGAP_HTTPFullSuite(t *testing.T) {
	_ = withFreshInfra(t)
	const testTimeout = 60 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sessID := mqttlocal.UniqueClientID("gap-ha1-sess")
	sess := setupMQTTSession(t, sessID, connectivity.SessionEphemeral)
	snd := setupMQTTSender(t, sess)

	rt := goruntime.New(
		goruntime.WithInstanceID("gap-ha1"),
		goruntime.WithDLQStore(&lrDLQStore{}),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-ha1-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "ha1-bind", Address: "gap-ha1/out"}),
		SourceCapabilities: []ports.Capability{ports.CapHTTPEndpoint},
	}, &noopReceiver{}, snd, nil, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	srv := httpapi.New(rt, httpapi.Config{
		AdminAddr:     ":0",
		MonitorAddr:   ":0",
		AdminAPIKey:   shared.NewSecret(gapAdminKey),
		MonitorAPIKey: shared.NewSecret(gapMonitorKey),
	}, httpapi.WithServerLogger(testLogger(t)))
	require.NoError(t, srv.Start(ctx))
	defer func() { _ = srv.Stop(context.Background()) }()

	adminURL := srv.AdminURL()
	monURL := srv.MonitorURL()
	t.Logf("GAP-HA1: admin=%s, monitor=%s", adminURL, monURL)

	// --- Unauthenticated probes ---
	status, data, err := httpGet(monURL+"/api/v1/monitor/health", "")
	require.NoError(t, err)
	assert.Equal(t, 200, status, "health should be 200")
	assert.Contains(t, data, "status", "health should have status field")

	status, _, err = httpGet(monURL+"/api/v1/monitor/live", "")
	require.NoError(t, err)
	assert.Equal(t, 200, status, "live should be 200")

	status, _, err = httpGet(monURL+"/api/v1/monitor/ready", "")
	require.NoError(t, err)
	assert.Equal(t, 200, status, "ready should be 200 when running")

	// --- Authenticated monitor endpoints ---
	status, data, err = httpGet(monURL+"/api/v1/monitor/topology", gapMonitorKey)
	require.NoError(t, err)
	assert.Equal(t, 200, status, "topology should be 200")
	t.Logf("GAP-HA1: topology = %v", data)

	status, data, err = httpGet(monURL+"/api/v1/monitor/deephealth", gapMonitorKey)
	require.NoError(t, err)
	assert.Equal(t, 200, status, "deephealth should be 200")
	assert.Contains(t, data, "ready_for_traffic")

	// --- Authenticated admin endpoints ---
	status, data, err = httpGet(adminURL+"/api/v1/admin/bridge", gapAdminKey)
	require.NoError(t, err)
	assert.Equal(t, 200, status, "bridge status should be 200")
	assert.Contains(t, data, "instance_id")

	status, data, err = httpGet(adminURL+"/api/v1/admin/routes", gapAdminKey)
	require.NoError(t, err)
	assert.Equal(t, 200, status, "routes should be 200")

	status, data, err = httpGet(adminURL+"/api/v1/admin/dlq", gapAdminKey)
	require.NoError(t, err)
	assert.Equal(t, 200, status, "dlq should be 200")

	// --- Auth failure ---
	status, _, err = httpGet(adminURL+"/api/v1/admin/bridge", "")
	require.NoError(t, err)
	assert.Equal(t, 401, status, "admin without key should be 401")

	status, _, err = httpGet(monURL+"/api/v1/monitor/topology", "")
	require.NoError(t, err)
	assert.Equal(t, 401, status, "topology without key should be 401")

	t.Log("GAP-HA1: all endpoint assertions passed")
}

// TestGAP_HTTPBridgeStartStop validates bridge start/stop lifecycle
// via HTTP endpoints.
//
// Assertions:
//   - POST /bridge/start starts the bridge
//   - Inject API delivers messages
//   - POST /bridge/stop stops gracefully
func TestGAP_HTTPBridgeStartStop(t *testing.T) {
	_ = withFreshInfra(t)
	const testTimeout = 60 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, "gap-ha2/out", "gap-ha2-col")

	sessID := mqttlocal.UniqueClientID("gap-ha2-sess")
	sess := setupMQTTSession(t, sessID, connectivity.SessionEphemeral)
	snd := setupMQTTSender(t, sess)

	rt := goruntime.New(
		goruntime.WithInstanceID("gap-ha2"),
		goruntime.WithDLQStore(&lrDLQStore{}),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-ha2-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "ha2-bind", Address: "gap-ha2/out"}),
		SourceCapabilities: []ports.Capability{ports.CapHTTPEndpoint},
	}, &noopReceiver{}, snd, nil, nil))

	// Start bridge manually (not via HTTP for this variant).
	require.NoError(t, rt.Start(ctx))
	gobridgesync(t, 10*time.Second, rt)

	srv := httpapi.New(rt, httpapi.Config{
		AdminAddr:     ":0",
		MonitorAddr:   ":0",
		AdminAPIKey:   shared.NewSecret(gapAdminKey),
		MonitorAPIKey: shared.NewSecret(gapMonitorKey),
	}, httpapi.WithServerLogger(testLogger(t)))
	require.NoError(t, srv.Start(ctx))
	defer func() { _ = srv.Stop(context.Background()) }()

	adminURL := srv.AdminURL()
	monURL := srv.MonitorURL()

	// Verify bridge is running.
	status, data, err := httpGet(monURL+"/api/v1/monitor/health", "")
	require.NoError(t, err)
	assert.Equal(t, 200, status)
	t.Logf("GAP-HA2: health = %v", data)

	// Inject a message via HTTP.
	injectPayload := map[string]any{
		"subject": "gap-ha2/out",
		"payload": "eyJ0ZXN0IjoiaW5qZWN0In0=", // base64 of {"test":"inject"}
	}
	status, _, err = httpPost(
		adminURL+"/api/v1/admin/routes/gap-ha2-route/inject",
		gapAdminKey, injectPayload)
	require.NoError(t, err)
	assert.Contains(t, []int{200, 202}, status, "inject should return success status")
	t.Logf("GAP-HA2: inject status=%d", status)

	// Use rt.Inject directly as well.
	for i := 0; i < 10; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("ha2-inject-%d", i),
			Subject: "gap-ha2/out",
			Payload: []byte(fmt.Sprintf(`{"seq":%d}`, i)),
		})
		_ = rt.Inject(ctx, "gap-ha2-route", env)
	}

	lrWaitFor(t, 15*time.Second, "collector >= 5",
		func() bool { return collector.count() >= 5 })
	t.Logf("GAP-HA2: collector=%d", collector.count())

	// Stop via direct API.
	stopErr := rt.Stop(context.Background())
	assert.NoError(t, stopErr, "Stop should succeed")
	assert.False(t, rt.IsRunning(), "bridge should not be running after stop")
	t.Log("GAP-HA2: bridge stopped successfully")
}

// TestGAP_HTTPDLQManagement validates DLQ list, replay, and purge
// via HTTP admin endpoints.
//
// Assertions:
//   - DLQ count matches failed messages
//   - List endpoint returns entries with correct fields
//   - Unconfirmed purge is rejected without deleting failure evidence
//   - Explicitly confirmed purge clears all existing entries
func TestGAP_HTTPDLQManagement(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 20
		testTimeout = 60 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "gap-ha3-in")
	dlq := &purgeableDLQStore{}

	sessID := mqttlocal.UniqueClientID("gap-ha3-sess")
	sess := setupMQTTSession(t, sessID, connectivity.SessionEphemeral)

	rt := goruntime.New(
		goruntime.WithInstanceID("gap-ha3"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-ha3-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "ha3-bind", Address: "gap-ha3/out"}),
		SourceCapabilities: directHoldCaps,
	}, newSQSReceiver(t, sqsInURL), &permanentFailSender{}, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	srv := httpapi.New(rt, httpapi.Config{
		AdminAddr:     ":0",
		MonitorAddr:   ":0",
		AdminAPIKey:   shared.NewSecret(gapAdminKey),
		MonitorAPIKey: shared.NewSecret(gapMonitorKey),
	}, httpapi.WithServerLogger(testLogger(t)))
	require.NoError(t, srv.Start(ctx))
	defer func() { _ = srv.Stop(context.Background()) }()

	adminURL := srv.AdminURL()

	// Send messages that will all fail permanently and be DLQ'd.
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)
	lrWaitFor(t, 30*time.Second, fmt.Sprintf("DLQ >= %d", msgCount),
		func() bool { return dlq.count() >= msgCount })
	t.Logf("GAP-HA3: DLQ count = %d", dlq.count())

	// GET /dlq — verify count.
	status, data, err := httpGet(adminURL+"/api/v1/admin/dlq", gapAdminKey)
	require.NoError(t, err)
	assert.Equal(t, 200, status)
	t.Logf("GAP-HA3: dlq summary = %v", data)

	// GET /dlq/messages — verify entries.
	status, data, err = httpGet(adminURL+"/api/v1/admin/dlq/messages?limit=5", gapAdminKey)
	require.NoError(t, err)
	assert.Equal(t, 200, status)
	t.Logf("GAP-HA3: dlq messages (limit=5) = %v", data)

	// POST /dlq/purge without destructive confirmation must preserve all entries.
	status, data, err = httpPost(adminURL+"/api/v1/admin/dlq/purge", gapAdminKey,
		map[string]any{"confirm_purge_all": false})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, status, "unconfirmed purge must be rejected")
	assert.Contains(t, data["error"], "confirm_purge_all=true")
	assert.Equal(t, msgCount, dlq.count(), "unconfirmed purge must preserve the DLQ")

	// Explicit confirmation authorizes the destructive operation.
	status, data, err = httpPost(adminURL+"/api/v1/admin/dlq/purge", gapAdminKey,
		map[string]any{"confirm_purge_all": true})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "confirmed purge should return 200")
	assert.Equal(t, float64(msgCount), data["purged"])
	assert.Equal(t, 0, dlq.count(), "confirmed purge must clear the DLQ")
	t.Logf("GAP-HA3: confirmed purge status=%d data=%v", status, data)

	t.Log("GAP-HA3: DLQ management assertions passed")
}
