package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
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
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
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

func seedDLQ(t *testing.T, store *memorydlq.Store, entries ...routing.DLQEntry) {
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
// claimed (deleted), re-injected into their routes, and the response
// reflects the correct counts.
//
// Flow (claim-before-inject):
// ───────────────────────────────────────────────
//
//	store.Get("e1") → store.Delete("e1") → rt.Inject("test-route")
//	store.Get("e2") → store.Delete("e2") → rt.Inject("test-route")
//
// ───────────────────────────────────────────────
func TestHandleDLQRedrive_AllSuccess(t *testing.T) {
	mux, dlq, sender := redriveSetup(t)
	seedDLQ(t, dlq,
		routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: "e1", RouteID: "test-route",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1", Payload: []byte("p1")}),
			FailedAt: time.Now(),
		}),
		routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: "e2", RouteID: "test-route",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s2", Payload: []byte("p2")}),
			FailedAt: time.Now(),
		}),
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
	remaining, _ := dlq.List(context.Background(), routing.DLQFilter{})
	assert.Empty(t, remaining)
}

// TestHandleDLQRedrive_EntryNotFound validates partial failure when
// some IDs don't exist in the DLQ store.
func TestHandleDLQRedrive_EntryNotFound(t *testing.T) {
	mux, dlq, _ := redriveSetup(t)
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "test-route",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
		FailedAt: time.Now(),
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1","missing-id"]}`))
	assert.Equal(t, http.StatusMultiStatus, rec.Code)

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
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "nonexistent-route",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
		FailedAt: time.Now(),
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1"]}`))
	assert.Equal(t, http.StatusMultiStatus, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(0), body["redriven"])
	assert.Equal(t, float64(1), body["failed"])

	errs := body["errors"].([]any)
	errObj := errs[0].(map[string]any)
	assert.Equal(t, "route not found", errObj["error"])

	// Claim-by-delete happens BEFORE inject; when inject fails the entry must
	// be restored (best-effort) so failure evidence is not silently lost.
	remaining, _ := dlq.List(context.Background(), routing.DLQFilter{})
	assert.Len(t, remaining, 1, "failed-inject entry must be restored to the DLQ")
}

// TestHandleDLQRedrive_FailedInject_RestoresDespiteCancelledRequest guards the
// detached-restore invariant: the best-effort restore after a failed inject runs
// under its OWN fresh, cancellation-immune context (context.WithoutCancel + a
// short timeout), independent of the request context AND of the per-batch
// budget. An operator disconnect mid-batch (or a batch that has exhausted its
// budget) must therefore NOT lose the claimed (already-deleted) entry.
func TestHandleDLQRedrive_FailedInject_RestoresDespiteCancelledRequest(t *testing.T) {
	mux, dlq, _ := redriveSetup(t)
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "nonexistent-route",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
		FailedAt: time.Now(),
	}))

	// Request context is already cancelled before the handler runs, simulating
	// an operator who disconnected mid-batch.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := redriveReq(`{"ids":["e1"]}`).WithContext(ctx)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// The inject still fails (route not found), and the restore still runs on a
	// detached context, so the claimed entry is returned to the DLQ rather than
	// permanently lost.
	remaining, _ := dlq.List(context.Background(), routing.DLQFilter{})
	assert.Len(t, remaining, 1, "claimed entry must be restored even when the request context is cancelled")
}

// TestHandleDLQRedrive_RetryDoesNotDoubleInject validates the claim-before-
// inject ordering: a second (retried) redrive of already-redriven IDs finds
// them gone and does NOT re-inject, matching the at-least-once/no-double-replay
// bias the fix targets.
func TestHandleDLQRedrive_RetryDoesNotDoubleInject(t *testing.T) {
	mux, dlq, sender := redriveSetup(t)
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "test-route",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
		FailedAt: time.Now(),
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1"]}`))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, sender.sentCount())

	// Retry the same request: entry already claimed+deleted, so no re-inject.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, redriveReq(`{"ids":["e1"]}`))
	assert.Equal(t, http.StatusMultiStatus, rec2.Code)
	assert.Equal(t, 1, sender.sentCount(), "retry must not double-inject")
}

// TestHandleDLQRedrive_DuplicateIDs validates that duplicate IDs in
// the request are deduplicated — the message is injected only once.
func TestHandleDLQRedrive_DuplicateIDs(t *testing.T) {
	mux, dlq, sender := redriveSetup(t)
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "test-route",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
		FailedAt: time.Now(),
	}))

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
	assert.Equal(t, http.StatusMultiStatus, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(0), body["redriven"])
	assert.Equal(t, float64(2), body["failed"])
}

// ═══════════════════════════════════════════════════════════════════
// Binding-scoped redrive threading (FIX 1 residual)
//
// injectRedrive must carry a DLQ entry's BindingID out-of-band into
// Runtime.InjectToBinding so a redrive of one failed leg of a fan-out
// route does not re-deliver to the N-1 healthy bindings. These tests
// pin that threading at the HTTP layer: a silent revert of injectRedrive
// back to a plain Inject would fail them.
// ═══════════════════════════════════════════════════════════════════

// recordingBindingInjectorRuntime wraps a real ports.Runtime (for DLQ
// read/admin access) and records Inject / InjectToBinding calls. Because it
// exposes InjectToBinding, injectRedrive's type-assertion to bindingInjector
// succeeds and a binding-scoped entry takes the confined path.
type recordingBindingInjectorRuntime struct {
	ports.Runtime
	mu           sync.Mutex
	injectRoutes []string
	bindingCalls []bindingInjectCall
}

type bindingInjectCall struct {
	routeID   string
	bindingID string
}

func (r *recordingBindingInjectorRuntime) Inject(_ context.Context, routeID string, _ *messaging.Envelope) error {
	r.mu.Lock()
	r.injectRoutes = append(r.injectRoutes, routeID)
	r.mu.Unlock()
	return nil
}

func (r *recordingBindingInjectorRuntime) InjectToBinding(_ context.Context, routeID, bindingID string, _ *messaging.Envelope) error {
	r.mu.Lock()
	r.bindingCalls = append(r.bindingCalls, bindingInjectCall{routeID: routeID, bindingID: bindingID})
	r.mu.Unlock()
	return nil
}

func newRecordingRedriveServer(t *testing.T) (*http.ServeMux, *memorydlq.Store, *recordingBindingInjectorRuntime) {
	t.Helper()
	dlq := memorydlq.NewStore()
	base := runtime.New(
		runtime.WithInstanceID("redrive-binding-test"),
		runtime.WithDLQStore(dlq),
	)
	rec := &recordingBindingInjectorRuntime{Runtime: base}
	s := New(rec, testConfig())
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)
	return mux, dlq, rec
}

// TestHandleDLQRedrive_ThreadsBindingIDIntoInjectToBinding asserts an entry
// carrying a BindingID is redriven via InjectToBinding with that EXACT binding,
// while an entry without one takes plain Inject.
func TestHandleDLQRedrive_ThreadsBindingIDIntoInjectToBinding(t *testing.T) {
	mux, dlq, rec := newRecordingRedriveServer(t)
	seedDLQ(t, dlq,
		routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: "with-binding", RouteID: "fanout-route", BindingID: "binding-A",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
			FailedAt: time.Now(),
		}),
		routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: "no-binding", RouteID: "simple-route",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s2"}),
			FailedAt: time.Now(),
		}),
	)

	out := httptest.NewRecorder()
	mux.ServeHTTP(out, redriveReq(`{"ids":["with-binding","no-binding"]}`))
	require.Equal(t, http.StatusOK, out.Code)

	// The binding-scoped entry must go through InjectToBinding with its exact
	// BindingID — NOT plain Inject (which would fan out to all bindings).
	require.Len(t, rec.bindingCalls, 1, "binding-scoped entry must use InjectToBinding")
	assert.Equal(t, "fanout-route", rec.bindingCalls[0].routeID)
	assert.Equal(t, "binding-A", rec.bindingCalls[0].bindingID)

	// The entry without a binding must take plain Inject.
	require.Len(t, rec.injectRoutes, 1, "entry without a binding must use plain Inject")
	assert.Equal(t, "simple-route", rec.injectRoutes[0])
}

// recordingInjectOnlyRuntime wraps a real ports.Runtime but deliberately does
// NOT implement InjectToBinding, so injectRedrive falls back to a fan-out
// Inject when an entry carries a binding.
type recordingInjectOnlyRuntime struct {
	ports.Runtime
	mu           sync.Mutex
	injectRoutes []string
}

func (r *recordingInjectOnlyRuntime) Inject(_ context.Context, routeID string, _ *messaging.Envelope) error {
	r.mu.Lock()
	r.injectRoutes = append(r.injectRoutes, routeID)
	r.mu.Unlock()
	return nil
}

// TestHandleDLQRedrive_FanoutFallback_LogsWarnWhenBindingScopeUnavailable pins
// the visibility guarantee: when the runtime cannot confine a replay to a single
// binding but the entry HAS one, the redrive falls back to a fan-out Inject and
// that confinement loss is logged at Warn.
func TestHandleDLQRedrive_FanoutFallback_LogsWarnWhenBindingScopeUnavailable(t *testing.T) {
	dlq := memorydlq.NewStore()
	base := runtime.New(
		runtime.WithInstanceID("redrive-nobinding-test"),
		runtime.WithDLQStore(dlq),
	)
	rt := &recordingInjectOnlyRuntime{Runtime: base}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s := New(rt, testConfig(), WithServerLogger(logger))
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "some-route", BindingID: "binding-A",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
		FailedAt: time.Now(),
	}))

	out := httptest.NewRecorder()
	mux.ServeHTTP(out, redriveReq(`{"ids":["e1"]}`))
	require.Equal(t, http.StatusOK, out.Code)

	// Fell back to a fan-out Inject (binding dropped) ...
	require.Len(t, rt.injectRoutes, 1)
	assert.Equal(t, "some-route", rt.injectRoutes[0])

	// ... and the confinement loss is visible in the logs.
	logOut := buf.String()
	assert.Contains(t, logOut, "runtime lacks binding-scoped injection")
	assert.Contains(t, logOut, "binding-A")
}
