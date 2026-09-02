package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/runtime"
)

// TestHandleDeepHealth_RolloutProjectionCarriesEveryDivergenceField pins the
// wire contract of the coordinated-rollout block.
//
// The block is what an operator reads when a cohort is split, and it is only
// useful if it carries the three questions divergence raises: WHO has converged
// (the confirm barrier's roster), WHEN this member last saw the row, and whether
// that observation is still current. A field that exists in the projection but
// never reaches the JSON is worse than an absent one — the deep-health page
// looks complete while the answer is missing.
func TestHandleDeepHealth_RolloutProjectionCarriesEveryDivergenceField(t *testing.T) {
	observedAt := time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)
	rt := runtime.New(runtime.WithInstanceID("dh-rollout"))
	cfg := testConfig()
	cfg.ConfigWatchProvider = func() ConfigWatchHealth {
		return ConfigWatchHealth{
			Degraded: true,
			Reason:   "cluster rollout generation 4 committed but not applied on this member",
			Rollout: &ClusterRolloutHealth{
				MemberID:         "node-a",
				Generation:       4,
				State:            "committed",
				ConfigVersion:    12,
				Epoch:            []string{"node-a", "node-b"},
				Acked:            []string{"node-a", "node-b"},
				Converged:        []string{"node-b"},
				CandidateStaged:  true,
				Applied:          false,
				ObservedAt:       observedAt,
				ObservationAgeMS: 9000,
				Stale:            true,
				LastError:        "rollout store read timed out",
			},
		}
	}
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deephealth", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var body deepHealthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(t, body.ConfigWatch)
	require.NotNil(t, body.ConfigWatch.Rollout)
	got := body.ConfigWatch.Rollout
	assert.Equal(t, []string{"node-b"}, got.Converged,
		"epoch minus converged is who the confirm barrier is still waiting for")
	assert.False(t, got.Applied, "committed AND not applied identifies the split member")
	assert.True(t, got.Stale)
	assert.Equal(t, observedAt, got.ObservedAt.UTC(),
		"the absolute instant is what makes two members' snapshots comparable")

	// The wire shape matters as much as the decoded struct: an operator greps the
	// raw JSON, and a renamed or dropped key breaks every runbook that names it.
	var raw struct {
		ConfigWatch struct {
			Rollout map[string]json.RawMessage `json:"rollout"`
		} `json:"config_watch"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	for _, key := range []string{"converged", "observed_at", "applied", "stale", "observation_age_ms"} {
		assert.Contains(t, raw.ConfigWatch.Rollout, key, "deep health must publish %q", key)
	}
}

// TestHandleDeepHealth_RolloutProjectionOmitsAnUnobservedInstant proves the
// timestamp is omitted rather than rendered as the zero year when this member
// has never managed to read the rollout row. A "0001-01-01" in an operator's
// dashboard reads as a clock fault, not as "no observation yet".
func TestHandleDeepHealth_RolloutProjectionOmitsAnUnobservedInstant(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("dh-rollout-unobserved"))
	cfg := testConfig()
	cfg.ConfigWatchProvider = func() ConfigWatchHealth {
		return ConfigWatchHealth{Rollout: &ClusterRolloutHealth{MemberID: "node-a"}}
	}
	s := New(rt, cfg)

	mux := http.NewServeMux()
	s.registerMonitorRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deephealth", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var raw struct {
		ConfigWatch struct {
			Rollout map[string]json.RawMessage `json:"rollout"`
		} `json:"config_watch"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.NotContains(t, raw.ConfigWatch.Rollout, "observed_at")
}
