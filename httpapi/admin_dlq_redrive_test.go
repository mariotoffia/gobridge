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
// route_id does not exist get a "route or binding not found" error.
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
	assert.Equal(t, "route or binding not found", errObj["error"])

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
// No redrive-safe injection: shared_outbox/binding entries are REFUSED
// ([HIGH-3])
//
// Without InjectRedrive a replay reuses the ORIGINAL envelope ID, which a
// shared_outbox route's retained (envelope_id, binding_id) dedup row swallows
// as a duplicate — a proven silent-loss path (the message is never re-sent yet
// the redrive would report success and DELETE the evidence). injectRedrive
// therefore REFUSES a binding-scoped entry when the runtime lacks InjectRedrive
// (no inject, no delete: the entry and its evidence are preserved). Only a
// DIRECT entry (no binding, so no dedup row to collide with) still replays via
// plain Inject. These tests pin that refusal at the HTTP layer.
// ═══════════════════════════════════════════════════════════════════

// recordingBindingInjectorRuntime wraps a real ports.Runtime (for DLQ
// read/admin access) and records Inject / InjectToBinding calls. It exposes
// InjectToBinding but NOT InjectRedrive: even with binding-confined injection,
// reusing the original envelope ID is unsafe on a shared_outbox route, so a
// binding entry must be REFUSED (never reaching InjectToBinding). The recorded
// bindingCalls slice lets a test assert InjectToBinding is never called.
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

// TestHandleDLQRedrive_NoRedriveSafe_RefusesBindingEntry_AllowsDirect pins
// [HIGH-3] test (a)+(c): against a runtime that lacks InjectRedrive (but DOES
// expose InjectToBinding), a binding-scoped entry is REFUSED — never injected
// (not even via InjectToBinding, whose original-ID replay a shared_outbox dedup
// row would swallow) and never deleted, so its evidence is preserved — while a
// DIRECT entry (no binding) still replays via plain Inject and is deleted.
//
// Mutation reasoning — revert injectRedrive to the old InjectToBinding/plain
// Inject fallback (delete the errRedriveUnsafeSharedOutbox refusal) and this
// test fails: the binding entry is injected (bindingCalls == 1) and deleted, the
// status is 200 not 207, and the evidence is gone.
func TestHandleDLQRedrive_NoRedriveSafe_RefusesBindingEntry_AllowsDirect(t *testing.T) {
	mux, dlq, rec := newRecordingRedriveServer(t)
	seedDLQ(t, dlq,
		routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: "with-binding", RouteID: "fanout-route", BindingID: "binding-A",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1"}),
			FailedAt: time.Now(),
		}),
		// A COLLISION-FREE direct entry: empty envelope ID and no dedup-id
		// header. This is the only direct shape a non-redrive-safe runtime may
		// still replay (injectToBinding assigns a fresh id, so no transport can
		// dedup it). An ID-bearing direct entry is covered by
		// TestHandleDLQRedrive_NoRedriveSafe_DirectEntryWithID_RefusedNotDeleted.
		routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: "no-binding", RouteID: "simple-route",
			Envelope: messaging.Envelope{},
			FailedAt: time.Now(),
		}),
	)

	out := httptest.NewRecorder()
	mux.ServeHTTP(out, redriveReq(`{"ids":["with-binding","no-binding"]}`))
	require.Equal(t, http.StatusMultiStatus, out.Code, "one refused entry ⇒ 207")

	var body map[string]any
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &body))
	assert.Equal(t, float64(1), body["redriven"], "only the direct entry redrove")
	assert.Equal(t, float64(1), body["failed"], "the binding entry was refused")

	// The binding-scoped entry must NEVER reach InjectToBinding: reusing the
	// original ID is exactly the shared_outbox dedup silent-loss path.
	require.Len(t, rec.bindingCalls, 0, "binding entry must be refused, not injected via InjectToBinding")

	// The direct entry took plain Inject.
	require.Len(t, rec.injectRoutes, 1, "direct entry must use plain Inject")
	assert.Equal(t, "simple-route", rec.injectRoutes[0])

	// The refusal carries a clear, actionable per-entry error.
	errs, ok := body["errors"].([]any)
	require.True(t, ok)
	require.Len(t, errs, 1)
	errObj := errs[0].(map[string]any)
	assert.Equal(t, "with-binding", errObj["id"])
	assert.Contains(t, errObj["error"], "redrive-safe injection")

	// The refused binding entry is preserved; the direct entry was deleted.
	remaining, _ := dlq.List(context.Background(), routing.DLQFilter{})
	require.Len(t, remaining, 1, "refused binding entry must be preserved")
	assert.Equal(t, "with-binding", remaining[0].ID())
}

// recordingInjectOnlyRuntime wraps a real ports.Runtime and implements ONLY
// plain Inject — no InjectToBinding, no InjectRedrive. A direct entry redrives
// through it; a binding entry is refused ([HIGH-3]).
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

// TestHandleDLQRedrive_NoRedriveSafe_RefusesBindingEntry_NoInject pins [HIGH-3]
// test (a): a runtime that lacks BOTH InjectRedrive and InjectToBinding must
// REFUSE a binding entry outright — no inject at all (the pre-fix code fanned
// out to a plain Inject here, re-delivering to the route's other bindings) —
// preserve the entry, and log the refusal at Warn.
//
// Mutation reasoning — drop the errRedriveUnsafeSharedOutbox refusal and the
// handler falls back to a plain Inject (injectRoutes == 1), deletes the entry,
// and returns 200: the exact silent no-op-then-delete the finding describes.
func TestHandleDLQRedrive_NoRedriveSafe_RefusesBindingEntry_NoInject(t *testing.T) {
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
	require.Equal(t, http.StatusMultiStatus, out.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &body))
	assert.Equal(t, float64(0), body["redriven"])
	assert.Equal(t, float64(1), body["failed"])

	// Nothing was injected: the binding entry was refused, not fanned out.
	require.Len(t, rt.injectRoutes, 0, "a binding entry must not be injected without redrive-safe injection")

	// The entry (and its failure evidence) is preserved.
	remaining, _ := dlq.List(context.Background(), routing.DLQFilter{})
	require.Len(t, remaining, 1, "refused entry must be preserved")

	// The refusal is visible in the logs.
	logOut := buf.String()
	assert.Contains(t, logOut, "refusing shared_outbox/binding entry")
	assert.Contains(t, logOut, "binding-A")
}

// TestHandleDLQRedrive_NoRedriveSafe_DirectEntry_StillWorks pins [HIGH-3] test
// (c): a COLLISION-FREE direct entry (no binding, EMPTY envelope ID, no dedup-id
// header) has nothing an outbox or transport can dedup on — injectToBinding
// assigns a fresh id — so it still redrives via plain Inject and is deleted even
// when the runtime lacks InjectRedrive. The response carries the verify-delivery
// warning.
func TestHandleDLQRedrive_NoRedriveSafe_DirectEntry_StillWorks(t *testing.T) {
	dlq := memorydlq.NewStore()
	base := runtime.New(
		runtime.WithInstanceID("redrive-direct-test"),
		runtime.WithDLQStore(dlq),
	)
	rt := &recordingInjectOnlyRuntime{Runtime: base}
	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "direct-route", // no BindingID
		// Empty envelope ID and no dedup key: a fresh id is assigned at inject,
		// so no transport can silently deduplicate the replay.
		Envelope: messaging.Envelope{},
		FailedAt: time.Now(),
	}))

	out := httptest.NewRecorder()
	mux.ServeHTTP(out, redriveReq(`{"ids":["e1"]}`))
	require.Equal(t, http.StatusOK, out.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &body))
	assert.Equal(t, float64(1), body["redriven"])
	assert.Equal(t, float64(0), body["failed"])

	// The direct entry was injected (via plain Inject) and deleted.
	require.Len(t, rt.injectRoutes, 1)
	assert.Equal(t, "direct-route", rt.injectRoutes[0])
	remaining, _ := dlq.List(context.Background(), routing.DLQFilter{})
	assert.Empty(t, remaining, "a redriven direct entry must be deleted")

	// The non-redrive-safe caution is still surfaced for direct replays.
	assert.NotEmpty(t, body["warning"], "a non-redrive-safe direct replay must surface the verify-delivery warning")
}

// TestHandleDLQRedrive_NoRedriveSafe_DirectEntryWithID_RefusedNotDeleted pins
// [FINDING 3]: a DIRECT entry that carries a non-empty envelope ID (or a
// dedup-id header) must be REFUSED on a runtime lacking InjectRedrive — a plain
// Inject would reuse the original id/dedup key, which an idempotent/FIFO
// transport silently deduplicates (Send returns nil), and the handler would then
// delete the entry after a no-op. The entry (and its evidence) must be preserved:
// no inject, no delete, 207 + per-entry error.
//
// Mutation reasoning — revert injectRedrive's direct branch to an unconditional
// plain Inject (drop the errRedriveUnsafeNoFreshID refusal) and this test fails:
// the ID-bearing entry is injected (injectRoutes == 1) and deleted, 200 not 207.
func TestHandleDLQRedrive_NoRedriveSafe_DirectEntryWithID_RefusedNotDeleted(t *testing.T) {
	dlq := memorydlq.NewStore()
	base := runtime.New(
		runtime.WithInstanceID("redrive-direct-id-test"),
		runtime.WithDLQStore(dlq),
	)
	rt := &recordingInjectOnlyRuntime{Runtime: base}
	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	// A direct entry (no binding) whose envelope carries a real ID — exactly what
	// an SQS FIFO sender would hash into MessageDeduplicationId.
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "direct-route",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "orig-id-1", Subject: "s1"}),
		FailedAt: time.Now(),
	}))

	out := httptest.NewRecorder()
	mux.ServeHTTP(out, redriveReq(`{"ids":["e1"]}`))
	require.Equal(t, http.StatusMultiStatus, out.Code, "an ID-bearing direct entry must be refused ⇒ 207")

	var body map[string]any
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &body))
	assert.Equal(t, float64(0), body["redriven"])
	assert.Equal(t, float64(1), body["failed"])

	// Nothing injected, nothing deleted: the entry and its evidence survive.
	require.Len(t, rt.injectRoutes, 0, "an ID-bearing direct entry must not be injected without redrive-safe injection")
	remaining, _ := dlq.List(context.Background(), routing.DLQFilter{})
	require.Len(t, remaining, 1, "refused entry must be preserved")
	assert.Equal(t, "e1", remaining[0].ID())

	// The refusal carries a clear, actionable per-entry error.
	errs, ok := body["errors"].([]any)
	require.True(t, ok)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].(map[string]any)["error"], "redrive-safe injection")
}

// TestHandleDLQRedrive_RedriveSafe_BindingEntry_ProceedsAndDeletes pins [HIGH-3]
// test (b): when the runtime DOES implement redrive-safe injection
// (the concrete runtime.Runtime does), a binding-scoped entry whose binding
// still exists redrives via InjectRedrive (fresh ID + provenance) and is then
// deleted — the safe path proceeds end-to-end.
func TestHandleDLQRedrive_RedriveSafe_BindingEntry_ProceedsAndDeletes(t *testing.T) {
	mux, dlq, sender := redriveBoundSetup(t) // route "test-route" with live binding "b1"
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "e1", RouteID: "test-route", BindingID: "b1",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "orig-1", Subject: "s1", Payload: []byte("p1")}),
		FailedAt: time.Now(),
	}))

	out := httptest.NewRecorder()
	mux.ServeHTTP(out, redriveReq(`{"ids":["e1"]}`))
	require.Equal(t, http.StatusOK, out.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &body))
	assert.Equal(t, float64(1), body["redriven"])
	assert.Equal(t, float64(0), body["failed"])
	assert.Nil(t, body["warning"], "a redrive-safe runtime must not emit the verify-delivery warning")

	// The replay reached the live binding, and the entry was deleted.
	assert.Equal(t, 1, sender.sentCount(), "binding entry must be redriven to its live binding")
	remaining, _ := dlq.List(context.Background(), routing.DLQFilter{})
	assert.Empty(t, remaining, "a confirmed redrive must delete the DLQ entry")
}

// sentSnapshots returns clones of every envelope the stub sender delivered.
func (s *stubSender) sentSnapshots() []*messaging.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*messaging.Envelope, len(s.sent))
	for i, e := range s.sent {
		out[i] = e.Clone()
	}
	return out
}

// TestHandleDLQRedrive_UsesRedriveSafeInjection_FreshIDWithProvenance pins the
// D1 (CRITICAL) wiring: against a real runtime the redrive takes the
// redriveInjector capability, so the replay is re-issued under a FRESH
// envelope ID with the original ID preserved as x-bridge.causation-id.
// Reusing the original ID is a verified silent-loss path on shared_outbox
// routes (the outbox's retained dedup row swallows the re-persist while the
// redrive reports success and the DLQ entry is deleted).
func TestHandleDLQRedrive_UsesRedriveSafeInjection_FreshIDWithProvenance(t *testing.T) {
	mux, dlq, sender := redriveSetup(t)
	seedDLQ(t, dlq,
		routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: "e1", RouteID: "test-route",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
				ID: "orig-env-redrive-1", Subject: "s1", Payload: []byte("p1"),
				DeduplicationID: "orig-dedup", // [FINDING 2] must be stripped on redrive
			}),
			FailedAt: time.Now(),
		}),
	)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1"]}`))
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, float64(1), body["redriven"])

	sent := sender.sentSnapshots()
	require.Len(t, sent, 1)
	assert.NotEqual(t, "orig-env-redrive-1", sent[0].ID(),
		"redrive must re-issue under a fresh envelope ID (original collides with the outbox dedup row)")
	got, ok := sent[0].Header(messaging.HeaderCausationID)
	require.True(t, ok, "redriven message must carry provenance in %s", messaging.HeaderCausationID)
	assert.Equal(t, "orig-env-redrive-1", got)

	// [FINDING 2] the stale transport dedup key must NOT ride through — otherwise
	// a FIFO/idempotent sender would swallow the "fresh" redrive and the handler
	// would delete the entry after a no-op.
	if v, dup := sent[0].Header(messaging.HeaderDeduplicationID); dup {
		assert.NotEqual(t, "orig-dedup", v, "redrive must strip the original x-bridge.dedup-id")
	}
}
