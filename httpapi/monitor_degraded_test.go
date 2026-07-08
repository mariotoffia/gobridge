package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/runtime"
)

// TestHandleDeepHealth_ConfigWatchProjection asserts the deep-health endpoint
// additively surfaces live-reconfiguration health from the DegradedProvider so a
// bridge running blind on its last good config is observable to operators
// (Finding 4). The projection is omitted entirely when no provider is wired.
func TestHandleDeepHealth_ConfigWatchProjection(t *testing.T) {
	tests := []struct {
		name         string
		provider     func() (bool, string)
		wantPresent  bool
		wantDegraded bool
		wantReason   string
	}{
		{
			name:        "no provider omits the projection",
			provider:    nil,
			wantPresent: false,
		},
		{
			name:         "healthy provider reports not degraded",
			provider:     func() (bool, string) { return false, "" },
			wantPresent:  true,
			wantDegraded: false,
		},
		{
			name:         "degraded provider flips the field with a reason",
			provider:     func() (bool, string) { return true, "config change stream closed" },
			wantPresent:  true,
			wantDegraded: true,
			wantReason:   "config change stream closed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := runtime.New(runtime.WithInstanceID("dh-config-watch"))
			cfg := testConfig()
			cfg.DegradedProvider = tt.provider
			s := New(rt, cfg)

			mux := http.NewServeMux()
			s.registerMonitorRoutes(mux)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deephealth", nil)
			req.Header.Set("X-API-Key", "test-secret-key-0123456789")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			// Assert raw presence/absence so the omitempty additive contract is
			// verified, not just the decoded pointer.
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
			_, present := raw["config_watch"]
			assert.Equal(t, tt.wantPresent, present, "config_watch field presence")

			if tt.wantPresent {
				var body deepHealthResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				require.NotNil(t, body.ConfigWatch)
				assert.Equal(t, tt.wantDegraded, body.ConfigWatch.Degraded)
				assert.Equal(t, tt.wantReason, body.ConfigWatch.Reason)
			}
		})
	}
}
