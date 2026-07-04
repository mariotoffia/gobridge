package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestSlogAuditLogger_EmitsAllEventFields asserts the runtime audit logger
// mirrors the httpapi.SlogAuditLogger attribute contract (action, actor,
// resource, outcome, event_time, resource_id, detail keys) so runtime-side
// and admin-API audit lines query identically.
func TestSlogAuditLogger_EmitsAllEventFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	al := newSlogAuditLogger(logger)
	al.Log(context.Background(), ports.AuditEvent{
		Timestamp:  time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC),
		Action:     "lease.acquire",
		Actor:      "bridge-a",
		Resource:   "lease",
		ResourceID: "route-1",
		Outcome:    "success",
		Detail:     map[string]any{"attempt": 1},
	})

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	assert.Equal(t, "audit", line["msg"])
	assert.Equal(t, "lease.acquire", line["action"])
	assert.Equal(t, "bridge-a", line["actor"])
	assert.Equal(t, "lease", line["resource"])
	assert.Equal(t, "route-1", line["resource_id"])
	assert.Equal(t, "success", line["outcome"])
	assert.Contains(t, line, "event_time")
	assert.EqualValues(t, 1, line["attempt"])
}

// TestSlogAuditLogger_OmitsEmptyResourceID pins the optional field contract.
func TestSlogAuditLogger_OmitsEmptyResourceID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	newSlogAuditLogger(logger).Log(context.Background(), ports.AuditEvent{
		Action:  "dlq.redrive",
		Actor:   "bridge-a",
		Outcome: "success",
	})

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	assert.NotContains(t, line, "resource_id")
}
