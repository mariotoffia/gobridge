package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// A failed admin auth attempt must emit an audit event so credential
// brute-forcing is visible (previously invisible).
func TestRequireAdminAuth_FailureEmitsAudit(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("auth-audit"))
	audit := &recordingAuditLogger{}
	s := New(rt, testConfig(), WithAuditLogger(audit))

	h := s.requireAdminAuth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
	req.Header.Set("X-API-Key", "wrong-key")
	rec := httptest.NewRecorder()
	h(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	events := audit.Events()
	require.Len(t, events, 1)
	assert.Equal(t, "auth.failure", events[0].Action)
	assert.Equal(t, "failure", events[0].Outcome)
}

// After AuthFailureLimit failures within the window, the next attempt is
// throttled with 429 and a throttle audit event — even if it now presents the
// correct key (online guessing is slowed regardless).
func TestRequireAdminAuth_ThrottlesAfterLimit(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	rt := runtime.New(runtime.WithInstanceID("auth-throttle"))
	audit := &recordingAuditLogger{}
	cfg := testConfig()
	cfg.AuthFailureLimit = 3
	cfg.AuthFailureWindow = time.Minute
	s := New(rt, cfg, WithAuditLogger(audit), WithClock(clk))

	h := s.requireAdminAuth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	do := func(key string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		req.Header.Set("X-API-Key", key)
		// Stable client identity so the per-client counter accumulates.
		req.RemoteAddr = "10.0.0.9:1234"
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}

	for i := 0; i < 3; i++ {
		assert.Equal(t, http.StatusUnauthorized, do("wrong"))
	}
	// 4th attempt is throttled regardless of key correctness.
	assert.Equal(t, http.StatusTooManyRequests, do("test-secret-key-0123456789"))

	// After the window elapses the client may try again.
	clk.Advance(time.Minute + time.Second)
	assert.Equal(t, http.StatusOK, do("test-secret-key-0123456789"))
}

// A config PATCH carrying an unknown field (notably a plugin `options` block,
// which the def types tag json:"-") must be REJECTED loudly (400) rather than
// silently dropped and then erased from disk on commit.
func TestHandleConfigTxnPatch_RejectsUnknownOptionsField(t *testing.T) {
	cfg := sampleBridgeConfig()
	s, _ := newConfigTestServer(t, cfg)

	txnID := createTxn(t, s)

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPatch,
		"/api/v1/admin/config/transactions/"+txnID+"/patch")
	req.SetPathValue("txnID", txnID)
	req.Body = bodyReader(`{"sessions":[{"id":"sess-1","options":{"broker_url":"x"}}]}`)
	s.handleConfigTxnPatch(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// guardNoConfigLoss must reject a merged config where an entry that had a
// non-nil Config in base lost it (nil), and pass configs where nothing was
// lost.
func TestGuardNoConfigLoss(t *testing.T) {
	withCfg := func() *ports.BridgeConfig {
		c := &ports.BridgeConfig{}
		s := ports.SessionDef{ID: "sess-1", Transport: "mqtt"}
		s.SetDecoded(stubPluginConfig{}, nil)
		c.Sessions = []ports.SessionDef{s}
		return c
	}

	t.Run("config lost -> error", func(t *testing.T) {
		base := withCfg()
		merged := &ports.BridgeConfig{
			Sessions: []ports.SessionDef{{ID: "sess-1", Transport: "amqp"}},
		}
		err := guardNoConfigLoss(base, merged)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sess-1")
	})

	t.Run("config preserved -> ok", func(t *testing.T) {
		base := withCfg()
		merged := withCfg()
		assert.NoError(t, guardNoConfigLoss(base, merged))
	})

	t.Run("base had no config -> ok", func(t *testing.T) {
		base := &ports.BridgeConfig{Sessions: []ports.SessionDef{{ID: "sess-1", Transport: "mqtt"}}}
		merged := &ports.BridgeConfig{Sessions: []ports.SessionDef{{ID: "sess-1", Transport: "mqtt"}}}
		assert.NoError(t, guardNoConfigLoss(base, merged))
	})
}

// Setting only one of TLSCertFile/TLSKeyFile is a startup error; both-or-none.
func TestValidateConfig_TLSRequiresBothCertAndKey(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("tls-validate"))

	cfg := testConfig()
	cfg.TLSCertFile = "/etc/cert.pem" // key missing
	s := New(rt, cfg)
	require.Error(t, s.validateConfig())

	cfg2 := testConfig()
	cfg2.TLSKeyFile = "/etc/key.pem" // cert missing
	s2 := New(rt, cfg2)
	require.Error(t, s2.validateConfig())

	cfg3 := testConfig() // neither -> plaintext ok
	s3 := New(rt, cfg3)
	require.NoError(t, s3.validateConfig())
}

// A commit succeeds durably (disk write) but the in-band ConfigApplier failing
// must surface as 500 committed_not_applied with the version — proving the
// "Commit writes the file but never applies it" fix wires the applier and
// reports divergence instead of a false "committed".
func TestHandleConfigTxnCommit_ApplierFailureReportsDivergence(t *testing.T) {
	cfg := sampleBridgeConfig()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, parser.WriteFile(path, cfg))

	rt := runtime.New(runtime.WithInstanceID("applier-test"))
	apiCfg := testConfig()
	apiCfg.ConfigStore = &parser.FileStore{Path: path, Registry: newTestRegistry(t)}
	apiCfg.ConfigProvider = func() *ports.BridgeConfig { return cfg }
	apiCfg.ConfigApplier = func(_ context.Context, _ *ports.BridgeConfig) error {
		return errors.New("runtime rejected the new config")
	}
	s := New(rt, apiCfg)

	txnID := createTxn(t, s)

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions/"+txnID+"/commit")
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnCommit(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "committed_not_applied")

	// The durable write still happened: disk carries the bumped version.
	parsed, err := parser.ParseFile(path, parser.FormatYAML, newTestRegistry(t))
	require.NoError(t, err)
	assert.Equal(t, 1, parsed.Version)
}

// A successful ConfigApplier is invoked with the committed config and the
// commit reports success.
func TestHandleConfigTxnCommit_ApplierSuccessApplies(t *testing.T) {
	cfg := sampleBridgeConfig()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, parser.WriteFile(path, cfg))

	var appliedVersion int
	rt := runtime.New(runtime.WithInstanceID("applier-ok-test"))
	apiCfg := testConfig()
	apiCfg.ConfigStore = &parser.FileStore{Path: path, Registry: newTestRegistry(t)}
	apiCfg.ConfigProvider = func() *ports.BridgeConfig { return cfg }
	apiCfg.ConfigApplier = func(_ context.Context, applied *ports.BridgeConfig) error {
		appliedVersion = applied.Version
		return nil
	}
	s := New(rt, apiCfg)

	txnID := createTxn(t, s)
	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions/"+txnID+"/commit")
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnCommit(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, appliedVersion, "applier must receive the committed version")
}
