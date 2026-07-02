package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStrictJSON_RejectsTrailingAndMultiValue is the B9-FU-STRICTJSON
// regression: decodeStrictJSON must reject a body that carries a valid
// leading JSON value followed by trailing garbage (e.g. `{"ttl":"90s"}JUNK`)
// or a second JSON value, instead of silently honoring the leading value.
// It is exercised through the real HTTP handlers for both the "empty body =>
// defaults" create path (which tolerates io.EOF) and the "body required"
// patch path, proving the trailing-data rejection is consistent across
// handlers.
func TestStrictJSON_RejectsTrailingAndMultiValue(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"trailing garbage after value", `{"ttl":"90s"}JUNK`},
		{"trailing second object", `{"ttl":"90s"}{"ttl":"1s"}`},
		{"trailing scalar token", `{"ttl":"90s"} 42`},
	}

	t.Run("create handler", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s, _ := newConfigTestServer(t, sampleBridgeConfig())

				rec := httptest.NewRecorder()
				req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions")
				req.Body = bodyReader(tc.body)
				req.Header.Set("Content-Type", "application/json")
				s.handleConfigTxnCreate(rec, req)

				require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
				var out map[string]any
				require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
				assert.Equal(t, "invalid request body", out["error"])
			})
		}
	})

	// The patch path uses a config-shaped overlay, so reuse a bridge body with
	// the same trailing-data shapes to prove the sibling handler rejects too.
	patchCases := []struct {
		name string
		body string
	}{
		{"trailing garbage after value", `{"bridge":{"id":"test-bridge"}}JUNK`},
		{"trailing second object", `{"bridge":{"id":"test-bridge"}}{"bridge":{"id":"x"}}`},
	}
	t.Run("patch handler", func(t *testing.T) {
		for _, tc := range patchCases {
			t.Run(tc.name, func(t *testing.T) {
				s, _ := newConfigTestServer(t, sampleBridgeConfig())
				txnID := createTxn(t, s)

				rec := httptest.NewRecorder()
				req := adminRequest(http.MethodPatch, "/api/v1/admin/config/transactions/"+txnID)
				req.Body = bodyReader(tc.body)
				req.SetPathValue("txnID", txnID)
				s.handleConfigTxnPatch(rec, req)

				require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
				var out map[string]any
				require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
				assert.Equal(t, "invalid request body", out["error"])
			})
		}
	})
}
