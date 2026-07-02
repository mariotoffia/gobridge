package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCORS_PreflightAllowsConfigTxnMethods verifies the CORS preflight
// (OPTIONS) response advertises every HTTP verb the admin config
// transaction routes actually serve. PATCH (apply overlay) and DELETE
// (rollback) were previously omitted from Access-Control-Allow-Methods,
// so browser-based admin clients failed preflight for those operations.
//
// The expected verb set is the union of the config-transaction route
// registrations in admin_config.go:
//
//	POST   /config/transactions              (create)
//	GET    /config/transactions/{txnID}      (get)
//	PATCH  /config/transactions/{txnID}      (patch)
//	POST   /config/transactions/{txnID}/commit
//	DELETE /config/transactions/{txnID}      (rollback)
//
// plus OPTIONS for the preflight itself.
func TestCORS_PreflightAllowsConfigTxnMethods(t *testing.T) {
	rt := testRuntime()
	cfg := testConfig()
	cfg.CORSOrigins = "https://admin.example.com"
	s := New(rt, cfg)

	// corsMW is a shared, path-independent middleware; the wrapped mux
	// only needs the CORS layer engaged (CORSOrigins set). Preflight
	// short-circuits before the mux, so the monitor mux suffices.
	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	handler := s.wrap(mux)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/admin/config/transactions/txn-1", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)

	allowed := parseAllowMethods(rec.Header().Get("Access-Control-Allow-Methods"))

	for _, verb := range []string{
		http.MethodGet,
		http.MethodPost,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodOptions,
	} {
		assert.Contains(t, allowed, verb,
			"CORS preflight must advertise %s for config-transaction clients", verb)
	}

	// The API serves no PUT/HEAD route; preflight must not over-advertise.
	assert.NotContains(t, allowed, http.MethodPut)
	assert.NotContains(t, allowed, http.MethodHead)
}

// parseAllowMethods splits an Access-Control-Allow-Methods header value
// into a set of upper-cased verbs for order-independent assertions.
func parseAllowMethods(header string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, m := range strings.Split(header, ",") {
		if v := strings.ToUpper(strings.TrimSpace(m)); v != "" {
			set[v] = struct{}{}
		}
	}
	return set
}
