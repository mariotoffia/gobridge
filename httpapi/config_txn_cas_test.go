package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// ─────────────────────────────────────────────────────────────────────────────
// c1-txn-cas — config transaction commit must be an ATOMIC compare-and-swap
// across cluster instances. A plain read-check-Save has a lost-update window
// between the version read and the write; two instances committing from the
// same base version against a shared backend would each pass the guard and the
// second Save would clobber the first. When the store implements
// ports.ConditionalConfigStore the commit uses SaveIfVersion, which rejects a
// concurrent advance instead of overwriting it.
// ─────────────────────────────────────────────────────────────────────────────

// casConfigStore is a ports.ConditionalConfigStore fake. SaveIfVersion performs
// a genuine compare-and-swap against the currently-stored version; the plain
// Save is last-writer-wins (no check), modelling a file-backed store. Either
// write path first applies a queued "concurrent commit" (concurrentCfg) under
// the store lock the instant before the write — the tightest form of the
// read-modify-write race: this instance read version N, a peer committed N+1,
// and now this instance is about to write. A plain Save clobbers the peer's
// commit; SaveIfVersion must reject it with shared.ErrVersionMismatch.
type casConfigStore struct {
	mu            sync.Mutex
	current       *ports.BridgeConfig
	concurrentCfg *ports.BridgeConfig // a peer's commit, applied once at the write boundary
	concurrentAt  int                 // save index at which the peer lands (0 = the first write)
	saves         []*ports.BridgeConfig
}

// applyConcurrentLocked simulates a peer cluster instance committing a different
// config in the tiny window between this instance's version read and its write.
// It fires at most once, at the configured save index (concurrentAt), so a test
// can land the peer at the COMMIT write (index 0) or later at the ROLLBACK write
// (index 1). Must be called with mu held.
func (s *casConfigStore) applyConcurrentLocked() {
	if s.concurrentCfg != nil && len(s.saves) == s.concurrentAt {
		s.current = cloneBridgeConfig(s.concurrentCfg)
		s.concurrentCfg = nil
	}
}

func (s *casConfigStore) Load(_ context.Context) (*ports.BridgeConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil, fs.ErrNotExist
	}
	clone := *s.current
	return &clone, nil
}

func (s *casConfigStore) Save(_ context.Context, cfg *ports.BridgeConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyConcurrentLocked() // peer landed first; plain Save has no defence and clobbers it
	clone := *cfg
	s.current = &clone
	s.saves = append(s.saves, &clone)
	return nil
}

func (s *casConfigStore) SaveIfVersion(_ context.Context, cfg *ports.BridgeConfig, expected int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyConcurrentLocked() // peer landed first...
	stored := 0
	if s.current != nil {
		stored = s.current.Version
	}
	if stored != expected {
		// ...and the CAS refuses to overwrite the peer's newer version.
		return shared.ErrVersionMismatch
	}
	clone := *cfg
	s.current = &clone
	s.saves = append(s.saves, &clone)
	return nil
}

func (s *casConfigStore) Validate(_ context.Context, _ *ports.BridgeConfig) ([]string, error) {
	return nil, nil
}

func (s *casConfigStore) Merge(_ context.Context, _, overlay *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	clone := *overlay
	return &clone, nil
}

var _ ports.ConditionalConfigStore = (*casConfigStore)(nil)

// TestConfigTxnCommit_CAS_RejectsConcurrentVersionBump pins c1-txn-cas: a commit
// against a ConditionalConfigStore must use SaveIfVersion so a peer commit that
// advanced the shared version between this transaction's read and its write is
// REJECTED (errVersionConflict) rather than silently overwritten.
//
// Mutation reasoning — revert the SaveIfVersion CAS in commitDurable back to a
// plain m.store.Save and this test fails: the plain Save clobbers the peer's
// version-6 config with this transaction's version-6 write (last-writer-wins),
// Commit returns nil, and the on-disk config loses the peer's marker.
func TestConfigTxnCommit_CAS_RejectsConcurrentVersionBump(t *testing.T) {
	base := sampleBridgeConfig()
	base.Version = 5
	base.Bridge.LogLevel = "info"

	// The peer instance's already-committed config that must survive.
	peer := sampleBridgeConfig()
	peer.Version = 6
	peer.Bridge.LogLevel = "from-peer-instance"

	store := &casConfigStore{
		current:       cloneBridgeConfig(base),
		concurrentCfg: peer,
	}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	mgr := newTxnManager(store, func() *ports.BridgeConfig { return base }, nil, nil, clk)

	ctx := context.Background()
	txn, err := mgr.Begin(ctx, time.Minute)
	require.NoError(t, err)

	version, err := mgr.Commit(ctx, txn.ID)
	require.Error(t, err, "a commit that races a concurrent version bump must fail, not silently win")
	assert.ErrorIs(t, err, errVersionConflict)
	assert.Equal(t, 0, version)

	onDisk, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 6, onDisk.Version, "the peer's committed version must survive")
	assert.Equal(t, "from-peer-instance", onDisk.Bridge.LogLevel,
		"the peer's committed config content must NOT be clobbered by this transaction")
}

// TestConfigTxnCommit_CAS_SucceedsWhenNoConcurrentBump is the companion happy
// path: with no peer commit racing, the CAS commit succeeds and advances the
// version, so the fix does not break ordinary single-writer commits.
func TestConfigTxnCommit_CAS_SucceedsWhenNoConcurrentBump(t *testing.T) {
	base := sampleBridgeConfig()
	base.Version = 5

	store := &casConfigStore{current: cloneBridgeConfig(base)}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	mgr := newTxnManager(store, func() *ports.BridgeConfig { return base }, nil, nil, clk)

	ctx := context.Background()
	txn, err := mgr.Begin(ctx, time.Minute)
	require.NoError(t, err)

	version, err := mgr.Commit(ctx, txn.ID)
	require.NoError(t, err)
	assert.Equal(t, 6, version)

	onDisk, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 6, onDisk.Version)
}

// TestConfigTxnRollback_CAS_DoesNotClobberConcurrentAdvance pins the rollback
// half of c1-txn-cas: when an apply FAILS (non-in-flight) and the durable write
// is rolled back, that restore write must be as version-conditional as the
// forward commit. Here the commit lands version 6, the apply then fails, and a
// peer instance commits version 7 in the window before the rollback restore
// fires. The CAS restore must REFUSE (SaveIfVersion mismatch) rather than
// clobber the peer's acknowledged version-7 commit with the stale version-5
// prior.
//
// Mutation reasoning — revert restoreConfig to a plain m.store.Save and this
// test fails: the peer's version-7 config is overwritten by the version-5 prior
// (last-writer-wins), regressing the shared config and losing the peer's commit
// — exactly the lost-update class the CAS closes on the commit path.
func TestConfigTxnRollback_CAS_DoesNotClobberConcurrentAdvance(t *testing.T) {
	base := sampleBridgeConfig()
	base.Version = 5
	base.Bridge.LogLevel = "info"

	// A peer commit that lands AFTER this instance's commit (version 6) but
	// BEFORE its rollback restore — so it must survive the rollback.
	peer := sampleBridgeConfig()
	peer.Version = 7
	peer.Bridge.LogLevel = "from-peer-rollback"

	store := &casConfigStore{
		current:       cloneBridgeConfig(base),
		concurrentCfg: peer,
		concurrentAt:  1, // fire at the SECOND write: the rollback restore
	}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	applier := func(_ context.Context, _ *ports.BridgeConfig) error {
		// A genuine (NOT in-flight) apply rejection, so the durable write is
		// rolled back rather than retained.
		return fmt.Errorf("apply rejected: invalid route (not in-flight)")
	}
	mgr := newTxnManager(store, func() *ports.BridgeConfig { return base }, applier, nil, clk)

	ctx := context.Background()
	txn, err := mgr.Begin(ctx, time.Minute)
	require.NoError(t, err)

	_, err = mgr.Commit(ctx, txn.ID)
	require.Error(t, err, "the apply failed, so Commit must report an error")
	assert.ErrorIs(t, err, errConfigApplyFailed,
		"a rollback that could not restore (peer advanced) surfaces committed_not_applied")
	assert.NotErrorIs(t, err, errConfigRolledBack,
		"the rollback did NOT restore the prior config, so it is not a clean rolled_back")

	onDisk, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 7, onDisk.Version, "the peer's committed version must survive the rollback")
	assert.Equal(t, "from-peer-rollback", onDisk.Bridge.LogLevel,
		"the rollback must NOT clobber the peer's acknowledged commit with the stale prior")
}

// ─────────────────────────────────────────────────────────────────────────────
// c1-crash-unapplied — a commit whose in-band apply is merely IN-FLIGHT (the
// runtime accepted the config but its running state is not confirmed) must NOT
// roll the durable write back. ports.ErrApplyInFlight is the "committed, not
// confirmed applied, do NOT roll back" signal; the durable write is retained so
// a restart recovers the committed config, and the commit surfaces
// committed_not_applied semantics.
// ─────────────────────────────────────────────────────────────────────────────

// TestConfigTxnCommit_ApplyInFlight_DoesNotRollBack pins c1-crash-unapplied.
//
// Mutation reasoning — drop the ports.ErrApplyInFlight branch in Commit and the
// applier error falls through to rollbackAfterApplyFailure, which restores the
// previous on-disk config (version 4) and reports errConfigRolledBack. This test
// asserts the durable write is RETAINED at version 5 with only a single Save, so
// that regression fails it.
func TestConfigTxnCommit_ApplyInFlight_DoesNotRollBack(t *testing.T) {
	good := sampleBridgeConfig()
	good.Version = 4
	good.Bridge.LogLevel = "info"

	store := &recordingConfigStore{current: cloneBridgeConfig(good)}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	applier := func(_ context.Context, _ *ports.BridgeConfig) error {
		// The runtime accepted the config but the swap is still draining past the
		// apply deadline — a textbook ErrApplyInFlight terminal outcome.
		return fmt.Errorf("runtime swap still draining: %w", ports.ErrApplyInFlight)
	}
	mgr := newTxnManager(store, func() *ports.BridgeConfig { return good }, applier, nil, clk)

	ctx := context.Background()
	txn, err := mgr.Begin(ctx, time.Minute)
	require.NoError(t, err)

	version, err := mgr.Commit(ctx, txn.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ports.ErrApplyInFlight, "the in-flight signal must be propagated")
	assert.ErrorIs(t, err, errConfigApplyFailed, "an in-flight apply surfaces committed_not_applied")
	assert.NotErrorIs(t, err, errConfigRolledBack, "an in-flight apply must NOT roll back the durable write")
	assert.Equal(t, 5, version, "commit reports the committed (new) version, not a restored prior one")

	onDisk, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 5, onDisk.Version, "the durable write is retained while the apply is in-flight")
	require.Len(t, store.saves, 1, "exactly one Save (the durable commit); no rollback restore")
	assert.Equal(t, 5, store.saves[0].Version)
}

// TestHandleConfigTxnCommit_ApplyInFlight_Returns202Applying pins the HTTP
// mapping for c1-crash-unapplied: an in-flight apply is committed and converging,
// NOT a failure, so the commit endpoint must answer 202 committed_applying — a
// distinct, non-5xx outcome — rather than the generic 500 committed_not_applied
// used for a genuine apply failure. Collapsing both into 500 would let an
// operator/automation read "my change failed" and revert against a runtime that
// is already applying the change (the exact fight the durable-retain avoids).
//
// Mutation reasoning — drop the ports.ErrApplyInFlight branch in
// handleConfigTxnCommit and the error falls through to the errConfigApplyFailed
// branch: the response becomes 500 / committed_not_applied and this test fails.
func TestHandleConfigTxnCommit_ApplyInFlight_Returns202Applying(t *testing.T) {
	good := sampleBridgeConfig()
	good.Version = 4

	store := &recordingConfigStore{current: cloneBridgeConfig(good)}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	applier := func(_ context.Context, _ *ports.BridgeConfig) error {
		return fmt.Errorf("runtime swap still draining: %w", ports.ErrApplyInFlight)
	}
	mgr := newTxnManager(store, func() *ports.BridgeConfig { return good }, applier, nil, clk)

	s := newTestServer()
	s.configTxn = mgr

	txn, err := mgr.Begin(context.Background(), time.Minute)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions/"+txn.ID+"/commit")
	req.SetPathValue("txnID", txn.ID)
	s.handleConfigTxnCommit(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code,
		"an in-flight apply is committed+converging, not a 5xx failure")
	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "committed_applying", body["status"])
	assert.Equal(t, float64(5), body["version"], "the committed version is reported, not a restored prior")
}

// ─────────────────────────────────────────────────────────────────────────────
// c1-dlq-redrive-loss — redrive must INJECT before it DELETES. Deleting first
// opened an at-most-once loss window: a failed inject after the delete (and a
// failed best-effort restore) lost the message and its DLQ evidence. Injecting
// first and deleting only after a confirmed inject means a failed inject leaves
// the entry fully intact.
// ─────────────────────────────────────────────────────────────────────────────

// writeFailDLQStore is a DLQ store whose Write always fails, modelling the exact
// condition that made the old delete-first ordering lose data: after the entry
// was deleted, the best-effort restore Write could not put it back. Delete/Get/
// List delegate to the real memorydlq store.
type writeFailDLQStore struct {
	*memorydlq.Store
	writeErr error
}

func (s *writeFailDLQStore) Write(_ context.Context, _ routing.DLQEntry) error {
	return s.writeErr
}

// TestHandleDLQRedrive_InjectFailure_NeverLosesEntry pins c1-dlq-redrive-loss.
// The runtime cannot inject the entry (its recorded route no longer exists) AND
// the store's restore Write is rigged to fail. With inject-then-delete the entry
// is never deleted, so it survives.
//
// Mutation reasoning — restore the old claim-by-delete ordering (Delete before
// injectRedrive, with a best-effort restore) and this test fails: the Delete
// removes the entry, the inject fails, the restore Write fails, and the entry is
// permanently lost (List returns nothing).
func TestHandleDLQRedrive_InjectFailure_NeverLosesEntry(t *testing.T) {
	inner := memorydlq.NewStore()
	store := &writeFailDLQStore{Store: inner, writeErr: errors.New("dlq store write unavailable")}

	sender := newStubSender()
	recv := newStubReceiver()
	rt := runtime.New(
		runtime.WithInstanceID("redrive-loss-test"),
		runtime.WithDLQStore(store),
	)
	require.NoError(t, rt.AddRoute(runtime.RouteConfig{
		ID:                 "test-route",
		Policy:             routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}, recv, sender, nil, nil))
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	<-recv.ready

	// Seed via the INNER store — the wrapper's Write is rigged to fail, which is
	// exactly the restore-failure condition under test.
	require.NoError(t, inner.Write(context.Background(), routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:       "e1",
		RouteID:  "nonexistent-route", // makes injectRedrive fail with ErrNotFound
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s1", Payload: []byte("p1")}),
		FailedAt: time.Now(),
	})))

	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1"]}`))
	require.Equal(t, http.StatusMultiStatus, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(0), body["redriven"])
	assert.Equal(t, float64(1), body["failed"])

	remaining, err := inner.List(context.Background(), routing.DLQFilter{})
	require.NoError(t, err)
	require.Len(t, remaining, 1, "a failed inject must never lose the DLQ entry")
	assert.Equal(t, "e1", remaining[0].ID())
}

// ─────────────────────────────────────────────────────────────────────────────
// c1-apikey-weak — dynamic API-key providers must be validated on EVERY refresh
// with the same strength floor startup enforces. A rotation returning a weak
// (below-floor) key must be rejected and the last good key kept (fail closed),
// never installed.
// ─────────────────────────────────────────────────────────────────────────────

// TestAdminAuth_DynamicProviderRejectsWeakKey_KeepsLastGood pins c1-apikey-weak.
//
// Mutation reasoning — drop the validatedKeyProvider wrapping in New (revert to
// the bare shared.NewSecret(provider()) wrapper) and this test fails: the weak
// rotated key is installed, so presenting it authenticates (200 where 401 is
// asserted) and the previously-good strong key stops working (401 where 200 is
// asserted).
func TestAdminAuth_DynamicProviderRejectsWeakKey_KeepsLastGood(t *testing.T) {
	strong := "strong-admin-key-0123456789" // >= minAPIKeyLen
	weak := "short"                         // below minAPIKeyLen

	var current atomic.Pointer[string]
	current.Store(&strong)

	rt := testRuntime()
	cfg := Config{
		AdminAddr:           ":0",
		MonitorAddr:         ":0",
		AdminAPIKeyProvider: func() string { return *current.Load() },
	}
	s := New(rt, cfg)
	h := s.requireAdminAuth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	do := func(key string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		req.RemoteAddr = "10.9.9.9:5555"
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}

	// Establish the strong key as the last-good value.
	require.Equal(t, http.StatusOK, do(strong), "the strong key must authenticate")

	// The provider rotates to a below-floor key.
	current.Store(&weak)

	assert.Equal(t, http.StatusUnauthorized, do(weak),
		"a below-floor rotated key must be rejected, never installed")
	assert.Equal(t, http.StatusOK, do(strong),
		"the last valid key must remain in force after a rejected rotation (fail closed)")
}

// ─────────────────────────────────────────────────────────────────────────────
// c1-inject-deadline — admin inject and DLQ ops must apply a bounded backend
// deadline so a wedged runtime/store cannot hang the handler (and, through it,
// graceful shutdown) indefinitely on a patient client.
// ─────────────────────────────────────────────────────────────────────────────

// deadlineProbeRuntime wraps a real runtime and observes the context passed to
// Inject. When block is set it blocks until the context is cancelled and returns
// the cancellation error, standing in for a wedged backend.
type deadlineProbeRuntime struct {
	ports.Runtime
	block bool

	mu          sync.Mutex
	sawInject   bool
	hadDeadline bool
}

func (r *deadlineProbeRuntime) Inject(ctx context.Context, _ string, _ *messaging.Envelope) error {
	_, ok := ctx.Deadline()
	r.mu.Lock()
	r.sawInject = true
	r.hadDeadline = ok
	r.mu.Unlock()
	if r.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (r *deadlineProbeRuntime) injectObserved() (seen, deadline bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sawInject, r.hadDeadline
}

func injectDeadlineRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routes/r1/inject",
		strings.NewReader(`{"subject":"s","payload":""}`))
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.4.4.4:5555"
	return req
}

// TestHandleInject_AppliesBackendDeadline pins c1-inject-deadline with zero
// timing dependency: the backend inject must receive a context carrying a
// deadline, not the raw (deadline-less) request context.
//
// Mutation reasoning — revert handleInject to rt.Inject(r.Context(), ...) and
// this test fails: the httptest request context carries no deadline, so the
// probe records hadDeadline == false.
func TestHandleInject_AppliesBackendDeadline(t *testing.T) {
	base := runtime.New(runtime.WithInstanceID("inject-deadline-test"))
	rt := &deadlineProbeRuntime{Runtime: base}
	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, injectDeadlineRequest())
	require.Equal(t, http.StatusOK, rec.Code)

	seen, deadline := rt.injectObserved()
	require.True(t, seen, "the handler must reach the backend inject")
	assert.True(t, deadline, "the backend inject must receive a bounded (deadline) context")
}

// TestHandleInject_WedgedBackendFailsFast proves the applied deadline is
// effective: a backend that hangs until its context is cancelled must not hang
// the handler — it returns once the bounded deadline fires. Under the unfixed
// code (raw request context, which never cancels here) this call would block
// forever, which is the exact wedged-handler hazard the finding describes.
func TestHandleInject_WedgedBackendFailsFast(t *testing.T) {
	base := runtime.New(runtime.WithInstanceID("inject-wedge-test"))
	rt := &deadlineProbeRuntime{Runtime: base, block: true}
	cfg := testConfig()
	cfg.AdminOperationTimeout = 30 * time.Millisecond
	s := New(rt, cfg)
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, injectDeadlineRequest())

	// The handler returned (did not hang): the bounded context cancelled the
	// wedged backend and the inject failed fast.
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	seen, deadline := rt.injectObserved()
	require.True(t, seen)
	assert.True(t, deadline)
}
