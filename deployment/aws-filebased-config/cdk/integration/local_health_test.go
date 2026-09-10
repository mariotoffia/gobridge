//go:build integration_local

package integration

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigRefusal_DistinguishesPendingStartupFromRejection(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		refused    bool
	}{
		{"waiting", `{"empty":true,"config_watch":{"startup_pending":true}}`, false},
		{"retryable", `{"empty":true,"config_watch":{"startup_pending":true,"degraded":true,"reason":"temporary timeout","last_apply_error":"temporary timeout"}}`, false},
		{"invalid", `{"empty":true,"config_watch":{"last_apply_error":"invalid configuration"}}`, true},
		{"denied", `{"empty":true,"config_watch":{"degraded":true,"reason":"access denied"}}`, true},
		{"running", `{"empty":false,"config_watch":{"degraded":true,"reason":"temporary timeout"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var health deployedHealth
			require.NoError(t, json.Unmarshal([]byte(tc.body), &health))
			if tc.refused {
				require.Error(t, configRefusal(health))
			} else {
				require.NoError(t, configRefusal(health))
			}
		})
	}
}
