package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// liveProbe serves GET /live against the given server and returns the recorder.
func liveProbe(s *Server) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor/live", nil))
	return rec
}

// TestLive_WedgedSupervisorFailsClosed proves the liveness probe sees a
// composition root whose supervisor is WEDGED — a swap and its recovery both
// failed, so there is no active runtime and nothing is routed. Without a
// terminal signal of its own the probe only sees "no runtime", which is the
// same shape as a healthy swap window, so it answers 200 and the orchestrator
// never restarts a dead process.
func TestLiveness_WedgedSupervisorFailsClosed(t *testing.T) {
	wedged := false
	s := New(nil, Config{
		AdminAddr:       ":0",
		MonitorAddr:     ":0",
		AdminAPIKey:     shared.NewSecret("test-admin-key-1234567890"),
		RuntimeProvider: func() ports.Runtime { return nil },
		TerminalProvider: func() bool {
			return wedged
		},
	}, WithServerLogger(nil))

	require.Equal(t, http.StatusOK, liveProbe(s).Code,
		"a transient swap window (no runtime, not terminal) must stay alive")

	wedged = true

	rec := liveProbe(s)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"a wedged supervisor must fail the liveness probe so the orchestrator restarts the process")
	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "terminal", body["status"])
}

// TestCurrentRuntime_ProviderNilIsAuthoritative proves a configured
// RuntimeProvider OWNS the answer: when it reports no runtime the server must
// not silently serve the runtime handed to New(). That runtime is the process's
// long-stopped boot runtime, so falling back to it makes every monitor and
// admin endpoint answer from a dead object — boot routes on /topology, a closed
// store behind the DLQ endpoints — exactly while the process is wedged.
func TestLivenessRuntimeProvider_NilIsAuthoritative(t *testing.T) {
	boot := runtime.New(runtime.WithInstanceID("boot-runtime"))
	s := New(boot, Config{
		AdminAddr:       ":0",
		MonitorAddr:     ":0",
		AdminAPIKey:     shared.NewSecret("test-admin-key-1234567890"),
		RuntimeProvider: func() ports.Runtime { return nil },
	}, WithServerLogger(nil))

	assert.Nil(t, s.currentRuntime(),
		"a provider that reports no runtime must not be overridden by the boot runtime")
}

// TestCurrentRuntime_WithoutProviderUsesConstructorRuntime proves the fallback
// that still has a purpose: an embedder that wires no RuntimeProvider keeps
// being served by the runtime it passed to New().
func TestLivenessRuntimeProvider_AbsentProviderUsesConstructorRuntime(t *testing.T) {
	boot := runtime.New(runtime.WithInstanceID("boot-runtime"))
	s := New(boot, Config{
		AdminAddr:   ":0",
		MonitorAddr: ":0",
		AdminAPIKey: shared.NewSecret("test-admin-key-1234567890"),
	}, WithServerLogger(nil))

	assert.Same(t, ports.Runtime(boot), s.currentRuntime())
}
