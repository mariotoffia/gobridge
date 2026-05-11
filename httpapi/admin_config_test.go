package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// stubPluginConfig is the trivial PluginConfig returned by the test
// stub decoders; it has no fields and never fails validation.
type stubPluginConfig struct{ kind string }

func (s stubPluginConfig) Kind() string    { return s.kind }
func (s stubPluginConfig) Validate() error { return nil }

// newTestRegistry builds a hermetic *ports.Registry for the httpapi
// tests, pre-populated with stub decoders for the transport kinds
// the test fixtures reference. Each test that calls config.ParseFile
// supplies its own registry instance via this helper instead of
// relying on a process-wide singleton.
func newTestRegistry(t testing.TB) *ports.Registry {
	t.Helper()
	reg := ports.NewRegistry()
	for _, k := range []string{"mqtt", "sqs", "http"} {
		kind := k
		if err := reg.Register(kind, func(raw ports.RawConfig) (ports.PluginConfig, error) {
			return stubPluginConfig{kind: kind}, nil
		}); err != nil {
			t.Fatalf("register stub %q: %v", kind, err)
		}
	}
	return reg
}

// sampleBridgeConfig returns a minimal valid BridgeConfig for testing.
func sampleBridgeConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:              "test-bridge",
			DeploymentMode:  "standalone",
			ShutdownTimeout: "30s",
		},
		Sessions: []ports.SessionDef{
			{ID: "sess-1", Transport: "mqtt"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx-1", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx-1", Transport: "mqtt", SessionID: "sess-1"},
		},
		Bindings: []ports.BindingDef{
			{ID: "bind-1", SenderID: "tx-1", SessionID: "sess-1", Address: "topic/a"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "route-1",
				ReceiverID:   "rx-1",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"bind-1"},
			},
		},
		HTTP: &ports.HTTPConfig{
			AdminAPIKey:   "super-secret-admin-key-1234",
			MonitorAPIKey: "super-secret-monitor-key-5678",
		},
	}
}

// newConfigTestServer creates a Server wired for config API testing.
func newConfigTestServer(t *testing.T, cfg *ports.BridgeConfig, opts ...Option) (*Server, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Write initial config to disk.
	require.NoError(t, config.WriteFile(path, cfg))

	rt := runtime.New(runtime.WithInstanceID("config-test"))
	apiCfg := testConfig()
	apiCfg.ConfigStore = &config.FileStore{Path: path, Registry: newTestRegistry(t)}
	apiCfg.ConfigProvider = func() *ports.BridgeConfig { return cfg }

	s := New(rt, apiCfg, opts...)
	return s, path
}

// --- GET /config ---

func TestHandleConfigGet_ReturnsConfig(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	rec := httptest.NewRecorder()
	s.handleConfigGet(rec, adminRequest(http.MethodGet, "/api/v1/admin/config"))

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]json.RawMessage
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Contains(t, string(body["config"]), "test-bridge")
}

func TestHandleConfigGet_RedactsSensitiveFields(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	rec := httptest.NewRecorder()
	s.handleConfigGet(rec, adminRequest(http.MethodGet, "/api/v1/admin/config"))

	assert.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.NotContains(t, body, "super-secret-admin-key")
	assert.NotContains(t, body, "super-secret-monitor-key")
	assert.Contains(t, body, `"***"`)
}

// --- POST /config/transactions ---

func TestHandleConfigTxnCreate_Returns201(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions")
	req.Body = http.NoBody
	s.handleConfigTxnCreate(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.NotEmpty(t, body["txn_id"])
	assert.NotEmpty(t, body["expires_at"])
	assert.NotNil(t, body["config"])
}

func TestHandleConfigTxnCreate_WithTTL(t *testing.T) {
	cfg := sampleBridgeConfig()
	fixed := time.Date(2026, 5, 4, 1, 2, 3, 0, time.UTC)
	s, _ := newConfigTestServer(t, cfg, WithClock(clocktest.NewAt(fixed)))

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions")
	req.Body = bodyReader(`{"ttl": "2m"}`)
	req.Header.Set("Content-Type", "application/json")
	s.handleConfigTxnCreate(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))

	expiresAt, err := time.Parse(time.RFC3339, body["expires_at"].(string))
	require.NoError(t, err)
	createdAt, err := time.Parse(time.RFC3339, body["created_at"].(string))
	require.NoError(t, err)

	assert.Equal(t, fixed, createdAt)
	assert.Equal(t, fixed.Add(2*time.Minute), expiresAt)
}

func TestHandleConfigTxnCreate_Conflict(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	// Create first transaction.
	rec1 := httptest.NewRecorder()
	req1 := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions")
	req1.Body = http.NoBody
	s.handleConfigTxnCreate(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code)

	// Attempt second — should conflict.
	rec2 := httptest.NewRecorder()
	req2 := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions")
	req2.Body = http.NoBody
	s.handleConfigTxnCreate(rec2, req2)

	assert.Equal(t, http.StatusConflict, rec2.Code)
}

// --- PATCH /config/transactions/{txnID} ---

func TestHandleConfigTxnPatch_ReturnsMergedPreview(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	txnID := createTxn(t, s)

	// Patch: change bridge log_level.
	patch := `{"bridge": {"id": "test-bridge", "log_level": "debug"}}`
	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPatch, "/api/v1/admin/config/transactions/"+txnID)
	req.Body = bodyReader(patch)
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnPatch(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, txnID, body["txn_id"])

	preview := body["preview"].(map[string]any)
	bridge := preview["bridge"].(map[string]any)
	assert.Equal(t, "debug", bridge["log_level"])
	// Original fields preserved.
	assert.Equal(t, "test-bridge", bridge["id"])
}

func TestHandleConfigTxnPatch_AccumulatesMultiple(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	txnID := createTxn(t, s)

	// First patch: add a session.
	patch1 := `{"sessions": [{"id": "sess-2", "transport": "sqs"}]}`
	applyPatch(t, s, txnID, patch1)

	// Second patch: add a sender.
	patch2 := `{"senders": [{"id": "tx-2", "transport": "sqs"}]}`
	rec := applyPatch(t, s, txnID, patch2)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	preview := body["preview"].(map[string]any)

	sessions := preview["sessions"].([]any)
	senders := preview["senders"].([]any)
	assert.Len(t, sessions, 2, "both original and new session")
	assert.Len(t, senders, 2, "both original and new sender")
}

func TestHandleConfigTxnPatch_WrongTxnID_Returns404(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	_ = createTxn(t, s)

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPatch, "/api/v1/admin/config/transactions/bad-id")
	req.Body = bodyReader(`{"bridge": {"id": "test-bridge"}}`)
	req.SetPathValue("txnID", "bad-id")
	s.handleConfigTxnPatch(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleConfigTxnPatch_InvalidConfig_Returns422(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	txnID := createTxn(t, s)

	// Patch that makes config invalid: route referencing non-existent receiver.
	patch := `{"routes": [{"id": "bad-route", "receiver_id": "nonexistent", "bindings": ["bind-1"]}]}`
	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPatch, "/api/v1/admin/config/transactions/"+txnID)
	req.Body = bodyReader(patch)
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnPatch(rec, req)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.NotNil(t, body["validation_errors"])
}

// --- GET /config/transactions/{txnID} ---

func TestHandleConfigTxnGet_ReturnsPreview(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	txnID := createTxn(t, s)
	applyPatch(t, s, txnID, `{"bridge": {"id": "test-bridge", "log_level": "warn"}}`)

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodGet, "/api/v1/admin/config/transactions/"+txnID)
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnGet(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, txnID, body["txn_id"])
	assert.Equal(t, float64(1), body["patch_count"])
	assert.NotNil(t, body["preview"])
}

// --- POST /config/transactions/{txnID}/commit ---

func TestHandleConfigTxnCommit_WritesFile(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, path := newConfigTestServer(t, cfg)

	txnID := createTxn(t, s)
	applyPatch(t, s, txnID, `{"bridge": {"id": "test-bridge", "log_level": "error"}}`)

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions/"+txnID+"/commit")
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnCommit(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "committed", body["status"])
	assert.Equal(t, float64(1), body["version"]) // first commit: 0 → 1

	// Verify file was actually written with version.
	parsed, err := config.ParseFile(path, config.FormatYAML, newTestRegistry(t))
	require.NoError(t, err)
	assert.Equal(t, "error", parsed.Bridge.LogLevel)
	assert.Equal(t, "test-bridge", parsed.Bridge.ID)
	assert.Equal(t, 1, parsed.Version)
}

func TestHandleConfigTxnCommit_NoTempFilesRemain(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, path := newConfigTestServer(t, cfg)

	txnID := createTxn(t, s)
	applyPatch(t, s, txnID, `{"bridge": {"id": "test-bridge", "log_level": "info"}}`)

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions/"+txnID+"/commit")
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnCommit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only config.yaml should exist")
}

// --- DELETE /config/transactions/{txnID} ---

func TestHandleConfigTxnRollback_Succeeds(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	txnID := createTxn(t, s)

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodDelete, "/api/v1/admin/config/transactions/"+txnID)
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnRollback(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "rolled_back", body["status"])
}

func TestHandleConfigTxnRollback_AllowsNewTransaction(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	txnID := createTxn(t, s)

	// Rollback.
	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodDelete, "/api/v1/admin/config/transactions/"+txnID)
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnRollback(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// New transaction should succeed.
	rec2 := httptest.NewRecorder()
	req2 := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions")
	req2.Body = http.NoBody
	s.handleConfigTxnCreate(rec2, req2)
	assert.Equal(t, http.StatusCreated, rec2.Code)
}

// --- Auto-expiry ---

func TestConfigTxn_AutoTimeout(t *testing.T) {
	cfg := sampleBridgeConfig()
	clk := clocktest.NewAt(time.Date(2026, 5, 4, 1, 2, 3, 4, time.UTC))
	s, _ := newConfigTestServer(t, cfg, WithClock(clk))

	// Create transaction with very short TTL.
	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions")
	req.Body = bodyReader(`{"ttl": "100ms"}`)
	req.Header.Set("Content-Type", "application/json")
	s.handleConfigTxnCreate(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	var createBody map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&createBody))
	txnID := createBody["txn_id"].(string)

	clk.Advance(101 * time.Millisecond)
	wait.Until(t, 2*time.Second, "transaction expired", func() bool {
		r := httptest.NewRecorder()
		rq := adminRequest(http.MethodPatch, "/api/v1/admin/config/transactions/"+txnID)
		rq.Body = bodyReader(`{"bridge": {"id": "test-bridge"}}`)
		rq.SetPathValue("txnID", txnID)
		s.handleConfigTxnPatch(r, rq)
		return r.Code == http.StatusNotFound
	})
}

// --- sanitizeConfig ---

// --- Helpers ---

// createTxn creates a config transaction and returns the txn ID.
func createTxn(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions")
	req.Body = http.NoBody
	s.handleConfigTxnCreate(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	return body["txn_id"].(string)
}

// applyPatch sends a PATCH request and returns the recorder.
func applyPatch(t *testing.T, s *Server, txnID, patchJSON string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPatch, "/api/v1/admin/config/transactions/"+txnID)
	req.Body = bodyReader(patchJSON)
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnPatch(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "patch failed: %s", rec.Body.String())
	return rec
}

// bodyReader wraps a string as an io.ReadCloser for request bodies.
func bodyReader(s string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(s))
}
