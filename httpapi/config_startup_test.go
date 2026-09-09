package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

func TestConfigStartupPendingClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		pending bool
	}{
		{"clean wait", nil, true},
		{"unavailable", shared.ErrUnavailable, true},
		{"deadline", context.DeadlineExceeded, true},
		{"connection refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"throttled backend", startupHTTPError(429), true},
		{"unavailable backend", startupHTTPError(503), true},
		{"missing infrastructure", startupHTTPError(400), false},
		{"authorization", shared.ErrNotAuthorized, false},
		{"invalid blueprint", &ports.BlueprintValidationError{Errors: []string{"bad route"}}, false},
		{"invalid wrapped timeout", shared.ErrInvalidConfig.Wrap(context.DeadlineExceeded), false},
		{"unclassified refusal", errors.New("invalid source document"), false},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.pending, ConfigStartupPending(tc.err)) })
	}
}

func TestDeepHealthWithoutRuntimePreservesStartupDiagnosis(t *testing.T) {
	for _, pending := range []bool{true, false} {
		s := New(nil, Config{ConfigWatchProvider: func() ConfigWatchHealth {
			return ConfigWatchHealth{Degraded: true, Reason: "startup diagnostic", StartupPending: pending}
		}})
		w := httptest.NewRecorder()
		s.handleDeepHealth(w, httptest.NewRequest(http.MethodGet, "/", nil))
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		var response struct {
			Empty       bool              `json:"empty"`
			ConfigWatch ConfigWatchHealth `json:"config_watch"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.True(t, response.Empty)
		require.True(t, response.ConfigWatch.Degraded)
		require.Equal(t, pending, response.ConfigWatch.StartupPending)
	}
}
