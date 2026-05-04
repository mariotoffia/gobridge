package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════
// POST /dlq/redrive
//
// Redrive flow: Get entry → Inject into route → Delete from DLQ
//
// These tests use a real runtime with a stub receiver/sender so that
// rt.Inject() works, plus a real memorydlq.Store seeded with entries.
// ═══════════════════════════════════════════════════════════════════

func redriveSetup(t *testing.T) (*http.ServeMux, *memorydlq.Store, *stubSender) {
	t.Helper()
	dlq := memorydlq.NewStore()
	sender := &stubSender{}
	recv := newStubReceiver()
	rt := runtime.New(
		runtime.WithInstanceID("redrive-test"),
		runtime.WithDLQStore(dlq),
	)
	cfg := runtime.RouteConfig{
		ID: "test-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	err := rt.AddRoute(cfg, recv, sender, nil, nil)
	require.NoError(t, err)
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	<-recv.ready // wait for receiver goroutine instead of sleeping

	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)
	return mux, dlq, sender
}

func seedDLQ(t *testing.T, store *memorydlq.Store, entries ...domain.DLQEntry) {
	t.Helper()
	for _, e := range entries {
		require.NoError(t, store.Write(context.Background(), e))
	}
}

func redriveReq(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/dlq/redrive",
		strings.NewReader(body))
	r.Header.Set("X-API-Key", "test-secret-key-0123456789")
	r.Header.Set("Content-Type", "application/json")
	return r
}

// TestHandleDLQRedrive_AllSuccess validates that valid entries are
// re-injected into their routes, deleted from the DLQ, and the
// response reflects the correct counts.
//
// Flow:
// ───────────────────────────────────────────────
//
//	store.Get("e1") → rt.Inject("test-route") → store.Delete("e1")
//	store.Get("e2") → rt.Inject("test-route") → store.Delete("e2")
//
// ───────────────────────────────────────────────
func TestHandleDLQRedrive_AllSuccess(t *testing.T) {
	mux, dlq, sender := redriveSetup(t)
	seedDLQ(t, dlq,
		domain.DLQEntry{
			ID: "e1", RouteID: "test-route",
			Envelope: domain.Envelope{Subject: "s1", Payload: []byte("p1")},
			FailedAt: time.Now(),
		},
		domain.DLQEntry{
			ID: "e2", RouteID: "test-route",
			Envelope: domain.Envelope{Subject: "s2", Payload: []byte("p2")},
			FailedAt: time.Now(),
		},
	)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1","e2"]}`))
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(2), body["redriven"])
	assert.Equal(t, float64(0), body["failed"])
	assert.Nil(t, body["errors"])

	// Verify entries were injected into the route
	assert.Equal(t, 2, sender.sentCount())

	// Verify entries were deleted from DLQ
	remaining, _ := dlq.List(context.Background(), domain.DLQFilter{})
	assert.Empty(t, remaining)
}

// TestHandleDLQRedrive_EntryNotFound validates partial failure when
// some IDs don't exist in the DLQ store.
func TestHandleDLQRedrive_EntryNotFound(t *testing.T) {
	mux, dlq, _ := redriveSetup(t)
	seedDLQ(t, dlq, domain.DLQEntry{
		ID: "e1", RouteID: "test-route",
		Envelope: domain.Envelope{Subject: "s1"},
		FailedAt: time.Now(),
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1","missing-id"]}`))
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(1), body["redriven"])
	assert.Equal(t, float64(1), body["failed"])

	errs := body["errors"].([]any)
	assert.Len(t, errs, 1)
	errObj := errs[0].(map[string]any)
	assert.Equal(t, "missing-id", errObj["id"])
	assert.Equal(t, "entry not found", errObj["error"])
}

// TestHandleDLQRedrive_RouteNotFound validates that entries whose
// route_id does not exist get a "route not found" error.
func TestHandleDLQRedrive_RouteNotFound(t *testing.T) {
	mux, dlq, _ := redriveSetup(t)
	seedDLQ(t, dlq, domain.DLQEntry{
		ID: "e1", RouteID: "nonexistent-route",
		Envelope: domain.Envelope{Subject: "s1"},
		FailedAt: time.Now(),
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1"]}`))
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(0), body["redriven"])
	assert.Equal(t, float64(1), body["failed"])

	errs := body["errors"].([]any)
	errObj := errs[0].(map[string]any)
	assert.Equal(t, "route not found", errObj["error"])
}

// TestHandleDLQRedrive_DuplicateIDs validates that duplicate IDs in
// the request are deduplicated — the message is injected only once.
func TestHandleDLQRedrive_DuplicateIDs(t *testing.T) {
	mux, dlq, sender := redriveSetup(t)
	seedDLQ(t, dlq, domain.DLQEntry{
		ID: "e1", RouteID: "test-route",
		Envelope: domain.Envelope{Subject: "s1"},
		FailedAt: time.Now(),
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1","e1","e1"]}`))
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(1), body["redriven"])

	// Injected exactly once despite 3 duplicate IDs
	assert.Equal(t, 1, sender.sentCount())
}

// TestHandleDLQRedrive_EmptyIDs validates 400 for empty id array.
func TestHandleDLQRedrive_EmptyIDs(t *testing.T) {
	mux, _, _ := redriveSetup(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":[]}`))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleDLQRedrive_ExceedsMaxIDs validates 400 when exceeding 100 IDs.
func TestHandleDLQRedrive_ExceedsMaxIDs(t *testing.T) {
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%d", i)
	}
	body, _ := json.Marshal(map[string][]string{"ids": ids})
	mux, _, _ := redriveSetup(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(string(body)))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleDLQRedrive_InvalidBody validates 400 for non-JSON body.
func TestHandleDLQRedrive_InvalidBody(t *testing.T) {
	mux, _, _ := redriveSetup(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq("not-json"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleDLQRedrive_AllFailed validates response when every entry
// fails to redrive.
func TestHandleDLQRedrive_AllFailed(t *testing.T) {
	mux, _, _ := redriveSetup(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["nope1","nope2"]}`))
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(0), body["redriven"])
	assert.Equal(t, float64(2), body["failed"])
}
