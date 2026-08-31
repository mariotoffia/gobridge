package integration_test

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// ===============================================================
// Retry-policy, session-timing and log-level bounds at the admin
// config boundary.
//
// These fields used to be checked only where the BUILDER consumes
// them — at apply, or at the next restart — so an invalid value
// passed the config transaction, was written durably, and only then
// failed onto the rollback/divergence path. The proof that matters
// end to end is therefore not just "rejected" but "rejected with the
// durable config untouched".
// ===============================================================

// TestConfigAPI_Patch_InvalidRetryBounds_RejectedBeforeDurableWrite walks each
// newly bounded field through a real admin transaction and asserts the patch is
// refused AND the config file on disk is byte-identical afterwards.
func TestConfigAPI_Patch_InvalidRetryBounds_RejectedBeforeDurableWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	routeOverlay := func(policy map[string]any) map[string]any {
		return map[string]any{
			"routes": []map[string]any{
				{
					"id":            "r1",
					"receiver_id":   "rx-1",
					"delivery_mode": "direct_hold",
					"bindings":      []string{"bind-1"},
					"policy":        policy,
				},
			},
		}
	}

	for _, tc := range []struct {
		name    string
		overlay map[string]any
		field   string
	}{
		{
			name:    "negative initial_interval",
			overlay: routeOverlay(map[string]any{"backoff": map[string]any{"initial_interval": "-1s"}}),
			field:   "backoff.initial_interval",
		},
		{
			name:    "negative max_interval",
			overlay: routeOverlay(map[string]any{"backoff": map[string]any{"max_interval": "-30s"}}),
			field:   "backoff.max_interval",
		},
		{
			name:    "decaying multiplier",
			overlay: routeOverlay(map[string]any{"backoff": map[string]any{"multiplier": 0.5}}),
			field:   "backoff.multiplier",
		},
		{
			name:    "jitter above one",
			overlay: routeOverlay(map[string]any{"backoff": map[string]any{"jitter": 1.5}}),
			field:   "backoff.jitter",
		},
		{
			name: "non-positive broker_health_step_down",
			overlay: map[string]any{
				"routes": []map[string]any{
					{
						"id":          "r1",
						"receiver_id": "rx-1",
						"bindings":    []string{"bind-1"},
						"session": map[string]any{
							"session_id":              "sess-1",
							"sender_id":               "tx-1",
							"broker_health_step_down": "0s",
						},
					},
				},
			},
			field: "broker_health_step_down",
		},
		{
			name:    "unknown log level",
			overlay: map[string]any{"bridge": map[string]any{"log_level": "verbose"}},
			field:   "bridge.log_level",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newConfigAPITestServer(t, baseConfigForAPI())
			before := readRawFile(t, srv.ConfigFilePath)

			txnID := createTransaction(t, srv.URL, testAdminAPIKey)
			url := fmt.Sprintf("%s/api/v1/admin/config/transactions/%s", srv.URL, txnID)
			resp, body := apiPatch(t, url, testAdminAPIKey, tc.overlay)

			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("got %d, want 422 — the value must be refused at the transaction, body: %v",
					resp.StatusCode, body)
			}
			errs, ok := body["validation_errors"].([]any)
			if !ok || len(errs) == 0 {
				t.Fatalf("response carries no validation_errors: %v", body)
			}
			if !validationErrorMentions(errs, tc.field) {
				t.Fatalf("validation errors must name %q, got %v", tc.field, errs)
			}

			if after := readRawFile(t, srv.ConfigFilePath); !bytes.Equal(before, after) {
				t.Fatal("the durable config changed on a REJECTED patch; validation must precede the write")
			}
		})
	}
}

// TestConfigAPI_Commit_ValidRetryBounds_Succeeds is the negative control: the
// same fields at legal values — including a multiplier of exactly 1 (a fixed
// retry interval) and an explicit jitter opt-out — commit normally, so the new
// bounds reject nothing an operator is entitled to configure.
func TestConfigAPI_Commit_ValidRetryBounds_Succeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"log_level": "warning"}, // the documented alias of warn
		"routes": []map[string]any{
			{
				"id":            "r1",
				"receiver_id":   "rx-1",
				"delivery_mode": "direct_hold",
				"bindings":      []string{"bind-1"},
				"policy": map[string]any{
					"backoff": map[string]any{
						"initial_interval": "1s",
						"max_interval":     "30s",
						"multiplier":       1.0,
						"jitter":           0.0,
					},
				},
			},
		},
	})
	commitTransaction(t, srv.URL, testAdminAPIKey, txnID)

	committed := readConfigFromDisk(t, srv.ConfigFilePath)
	if committed.Bridge.LogLevel != "warning" {
		t.Fatalf("bridge.log_level = %q, want the committed alias", committed.Bridge.LogLevel)
	}
	backoff := committed.Routes[0].Policy.Backoff
	if backoff.Multiplier != 1.0 {
		t.Fatalf("backoff.multiplier = %v, want 1.0", backoff.Multiplier)
	}
	// The explicit opt-out must survive the durable round-trip as a SET zero,
	// not as an omitted field that would silently take the jitter default back.
	if backoff.Jitter == nil {
		t.Fatal("an explicit jitter: 0 was dropped by the commit round-trip; the opt-out must persist")
	}
	if *backoff.Jitter != 0 {
		t.Fatalf("backoff.jitter = %v, want the explicit 0", *backoff.Jitter)
	}
}

// validationErrorMentions reports whether any decoded validation error names sub.
func validationErrorMentions(errs []any, sub string) bool {
	for _, e := range errs {
		if s, ok := e.(string); ok && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
