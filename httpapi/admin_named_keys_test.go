package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// Named admin key fixtures. All are >= minAPIKeyLen so the same configs pass
// validateConfig; the near-miss shares alice's length to exercise the
// constant-time compare path without any length-dependent short-circuit.
const (
	aliceKey       = "alice-key-0123456789"
	aliceNearMiss  = "alice-key-9876543210"
	bobKey         = "bob-key-0123456789ab"
	carolKey       = "carol-key-0123456789"
	legacyAdminKey = "legacy-admin-key-0123456789"
	mapAdminKey    = "map-admin-key-0123456789ab"
)

// namedKeyAuthHandler wraps a next handler that emits one audit event, so the
// captured event reflects requireAdminAuth's context actor through emitAudit.
func namedKeyAuthHandler(s *Server) http.HandlerFunc {
	return s.requireAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		s.emitAudit(r, "test.action", "test", "", "success", nil)
		w.WriteHeader(http.StatusOK)
	})
}

// TestAdminAuth_NamedKeyMatch_ActorIsKeyName pins the core WP-API-IDENTITY
// behavior: a request authenticated with a named key attributes the audit
// Actor to that key NAME, and the (spoofable) network address demotes to
// Detail["client_addr"].
func TestAdminAuth_NamedKeyMatch_ActorIsKeyName(t *testing.T) {
	rt := testRuntime()
	audit := &recordingAuditLogger{}
	cfg := Config{
		AdminAddr:    ":0",
		MonitorAddr:  ":0",
		AdminAPIKeys: map[string]shared.Secret{"alice": shared.NewSecret(aliceKey)},
	}
	s := New(rt, cfg, WithAuditLogger(audit))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
	req.Header.Set("X-API-Key", aliceKey)
	req.RemoteAddr = "10.1.2.3:5544"
	rec := httptest.NewRecorder()
	namedKeyAuthHandler(s)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	events := audit.Events()
	require.Len(t, events, 1)
	assert.Equal(t, "alice", events[0].Actor, "actor must be the matched key name")
	require.NotNil(t, events[0].Detail)
	assert.Equal(t, "10.1.2.3:5544", events[0].Detail["client_addr"],
		"network address must be demoted to Detail[client_addr]")
}

// TestAdminAuth_LegacySingleKey_ActorIsAdmin verifies the back-compat folding:
// the legacy single AdminAPIKey authenticates under the reserved name "admin".
func TestAdminAuth_LegacySingleKey_ActorIsAdmin(t *testing.T) {
	rt := testRuntime()
	audit := &recordingAuditLogger{}
	// testConfig sets only the legacy AdminAPIKey.
	s := New(rt, testConfig(), WithAuditLogger(audit))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	namedKeyAuthHandler(s)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	events := audit.Events()
	require.Len(t, events, 1)
	assert.Equal(t, "admin", events[0].Actor)
}

// TestAdminAuth_MapOverridesLegacyOnCollision pins the precedence rule: when
// both the legacy AdminAPIKey and an explicit "admin" map entry are present,
// the map's key wins and the legacy value no longer authenticates.
func TestAdminAuth_MapOverridesLegacyOnCollision(t *testing.T) {
	rt := testRuntime()
	cfg := Config{
		AdminAddr:    ":0",
		MonitorAddr:  ":0",
		AdminAPIKey:  shared.NewSecret(legacyAdminKey),
		AdminAPIKeys: map[string]shared.Secret{"admin": shared.NewSecret(mapAdminKey)},
	}
	s := New(rt, cfg)
	h := namedKeyAuthHandler(s)

	do := func(key string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		req.Header.Set("X-API-Key", key)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusOK, do(mapAdminKey), "map's admin key must authenticate")
	assert.Equal(t, http.StatusUnauthorized, do(legacyAdminKey), "overridden legacy key must be rejected")
}

// TestAdminAuth_WrongKey_Unauthorized verifies a same-length near-miss key is
// rejected and that the auth.failure event — emitted before any successful
// match — keeps the address-based actor (no key identity by definition).
func TestAdminAuth_WrongKey_Unauthorized(t *testing.T) {
	rt := testRuntime()
	audit := &recordingAuditLogger{}
	cfg := Config{
		AdminAddr:    ":0",
		MonitorAddr:  ":0",
		AdminAPIKeys: map[string]shared.Secret{"alice": shared.NewSecret(aliceKey)},
	}
	require.Equal(t, len(aliceKey), len(aliceNearMiss), "near-miss must share alice's length")
	s := New(rt, cfg, WithAuditLogger(audit))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
	req.Header.Set("X-API-Key", aliceNearMiss)
	req.RemoteAddr = "10.9.9.9:4444"
	rec := httptest.NewRecorder()
	namedKeyAuthHandler(s)(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	events := audit.Events()
	require.Len(t, events, 1)
	assert.Equal(t, "auth.failure", events[0].Action)
	assert.Equal(t, "10.9.9.9:4444", events[0].Actor, "failed auth actor is address-based")
	_, hasClientAddr := events[0].Detail["client_addr"]
	assert.False(t, hasClientAddr, "no key identity => no client_addr demotion")
}

// TestAdminAuth_KeyNameValidation rejects names that are empty, too long, or
// contain characters outside [a-z0-9._-] (they end up in audit logs / tags).
func TestAdminAuth_KeyNameValidation(t *testing.T) {
	cases := map[string]string{
		"empty name":    "",
		"too long (65)": strings.Repeat("a", 65),
		"uppercase":     "Alice",
		"space":         "a b",
		"slash":         "a/b",
	}
	for name, keyName := range cases {
		t.Run(name, func(t *testing.T) {
			rt := testRuntime()
			cfg := Config{
				AdminAddr:    ":0",
				MonitorAddr:  ":0",
				AdminAPIKeys: map[string]shared.Secret{keyName: shared.NewSecret(aliceKey)},
			}
			s := New(rt, cfg)
			err := s.validateConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid admin key name")
		})
	}
}

// TestAdminAuth_ShortNamedKey_Rejected applies the minAPIKeyLen floor per key
// in the folded set.
func TestAdminAuth_ShortNamedKey_Rejected(t *testing.T) {
	rt := testRuntime()
	shortKey := "short-key-12345" // 15 chars, below minAPIKeyLen
	require.Less(t, len(shortKey), minAPIKeyLen)
	cfg := Config{
		AdminAddr:    ":0",
		MonitorAddr:  ":0",
		AdminAPIKeys: map[string]shared.Secret{"bob": shared.NewSecret(shortKey)},
	}
	s := New(rt, cfg)
	err := s.validateConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least")
}

// TestAdminAuth_ProviderMapReplacesStatic verifies AdminAPIKeysProvider fully
// replaces the static map per request (rotation semantics): the static key no
// longer authenticates and the provider's key does, attributing the new name.
func TestAdminAuth_ProviderMapReplacesStatic(t *testing.T) {
	rt := testRuntime()
	audit := &recordingAuditLogger{}
	cfg := Config{
		AdminAddr:    ":0",
		MonitorAddr:  ":0",
		AdminAPIKeys: map[string]shared.Secret{"alice": shared.NewSecret(aliceKey)}, // superseded by provider
		AdminAPIKeysProvider: func() map[string]string {
			return map[string]string{"bob": bobKey}
		},
	}
	s := New(rt, cfg, WithAuditLogger(audit))
	h := namedKeyAuthHandler(s)

	do := func(key string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		req.Header.Set("X-API-Key", key)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusUnauthorized, do(aliceKey), "static key must be replaced by provider")
	require.Equal(t, http.StatusOK, do(bobKey), "provider key must authenticate")

	events := audit.Events()
	require.NotEmpty(t, events)
	assert.Equal(t, "bob", events[len(events)-1].Actor)
}

// TestMonitorAuth_AdminNamedKeyActorFlows verifies the admin-superset fallback
// on a monitor endpoint still flows the matched key name into the context so
// audit attribution works there too.
func TestMonitorAuth_AdminNamedKeyActorFlows(t *testing.T) {
	rt := testRuntime()
	audit := &recordingAuditLogger{}
	cfg := Config{
		AdminAddr:    ":0",
		MonitorAddr:  ":0",
		AdminAPIKeys: map[string]shared.Secret{"carol": shared.NewSecret(carolKey)},
	}
	s := New(rt, cfg, WithAuditLogger(audit))

	h := s.requireMonitorAuth(func(w http.ResponseWriter, r *http.Request) {
		s.emitAudit(r, "test.monitor", "monitor", "", "success", nil)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/topology", nil)
	req.Header.Set("X-API-Key", carolKey)
	req.RemoteAddr = "10.2.2.2:6006"
	rec := httptest.NewRecorder()
	h(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	events := audit.Events()
	require.Len(t, events, 1)
	assert.Equal(t, "carol", events[0].Actor, "admin named key must attribute the monitor audit actor")
	assert.Equal(t, "10.2.2.2:6006", events[0].Detail["client_addr"])
}

// TestValidateAdminKeys pins the reload-boundary validator: it enforces the
// same tag-safe-name and minAPIKeyLen rules as startup validateConfig over a
// raw name->key map, and (deliberately) allows an empty/nil map so the
// "at least one key" guard stays owned by validateConfig.
func TestValidateAdminKeys(t *testing.T) {
	const longKey = "valid-key-0123456789" // 20 chars, >= minAPIKeyLen
	cases := []struct {
		name    string
		keys    map[string]string
		wantErr string
	}{
		{"valid map passes", map[string]string{"alice": longKey, "bob.1_2-x": longKey}, ""},
		{"empty map allowed", map[string]string{}, ""},
		{"nil map allowed", nil, ""},
		{"unsafe name rejected", map[string]string{"Ops/prod": longKey}, "invalid admin key name"},
		{"short key rejected", map[string]string{"alice": "short-key-12345"}, "at least"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAdminKeys(tc.keys)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
