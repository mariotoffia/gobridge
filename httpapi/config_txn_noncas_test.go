package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// TestConfigTxnCommit_NonCASStore_WithoutSingleWriter_Refused pins the
// root-cause fix at the manager boundary: a durable commit through a store that
// does NOT implement ports.ConditionalConfigStore, when single-writer was NOT
// asserted, must FAIL CLOSED with errConfigStoreNotCAS and perform NO Save. The
// old code fell through to a plain last-writer-wins Save, silently clobbering a
// peer instance's acknowledged commit on shared file/NFS/EFS config.
//
// Mutation reasoning — delete the `} else if !m.singleWriter {` refuse branch in
// commitDurable (so the non-CAS path falls straight through to m.store.Save) and
// this test fails: Commit returns nil, store.saves gains an entry, and disk is
// clobbered to version 4.
func TestConfigTxnCommit_NonCASStore_WithoutSingleWriter_Refused(t *testing.T) {
	good := sampleBridgeConfig()
	good.Version = 3

	store := &recordingConfigStore{current: cloneBridgeConfig(good)}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	mgr := newTxnManager(store, func() *ports.BridgeConfig { return good }, nil, nil, clk)
	// Simulate the real (multi-instance-possible) deployment posture that
	// Server.New installs from Config.ConfigSingleWriter: NOT single-writer.
	mgr.singleWriter = false

	ctx := context.Background()
	txn, err := mgr.Begin(ctx, time.Minute)
	require.NoError(t, err)

	version, err := mgr.Commit(ctx, txn.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, errConfigStoreNotCAS)
	assert.Zero(t, version)

	// The critical invariant: NO silent last-writer-wins Save happened.
	assert.Empty(t, store.saves, "a refused non-CAS commit must never write to disk")

	onDisk, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, onDisk.Version, "disk must be untouched by the refused commit")
}

// TestConfigTxnCommit_NonCASStore_SingleWriterOptIn_Saves documents the escape
// hatch: when the operator explicitly asserts a single admin writer, the plain
// Save path is taken (there is no peer to clobber). This is the ONLY sanctioned
// non-CAS durable write path.
func TestConfigTxnCommit_NonCASStore_SingleWriterOptIn_Saves(t *testing.T) {
	good := sampleBridgeConfig()
	good.Version = 3

	store := &recordingConfigStore{current: cloneBridgeConfig(good)}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	mgr := newTxnManager(store, func() *ports.BridgeConfig { return good }, nil, nil, clk)
	mgr.singleWriter = true // operator asserts sole-writer

	ctx := context.Background()
	txn, err := mgr.Begin(ctx, time.Minute)
	require.NoError(t, err)

	version, err := mgr.Commit(ctx, txn.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, version)

	require.Len(t, store.saves, 1, "single-writer commit persists via a plain Save")
	assert.Equal(t, 4, store.saves[0].Version)
}

// TestConfigTxnCommit_CASStore_CommitsWithoutSingleWriter proves the safe path
// is unaffected: a ports.ConditionalConfigStore commits through SaveIfVersion
// with no single-writer assertion, because the CAS write is atomic against the
// stored version and cannot silently clobber a peer.
func TestConfigTxnCommit_CASStore_CommitsWithoutSingleWriter(t *testing.T) {
	good := sampleBridgeConfig()
	good.Version = 3

	store := &casConfigStore{current: cloneBridgeConfig(good)}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	mgr := newTxnManager(store, func() *ports.BridgeConfig { return good }, nil, nil, clk)
	mgr.singleWriter = false // NOT asserted — CAS makes it safe regardless

	ctx := context.Background()
	txn, err := mgr.Begin(ctx, time.Minute)
	require.NoError(t, err)

	version, err := mgr.Commit(ctx, txn.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, version)

	require.Len(t, store.saves, 1, "CAS commit persists via SaveIfVersion")
	assert.Equal(t, 4, store.saves[0].Version)
}

// TestHandleConfigTxnCommit_NonCASStore_WithoutSingleWriter_Returns500 exercises
// the full path through Server.New wiring + the HTTP handler: a Server
// built with a non-CAS FileStore and Config.ConfigSingleWriter unset (the
// fail-closed default) must reject the durable commit with 500 and an actionable
// message instead of performing a silent last-writer-wins write to the file.
func TestHandleConfigTxnCommit_NonCASStore_WithoutSingleWriter_Returns500(t *testing.T) {
	cfg := sampleBridgeConfig()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, parser.WriteFile(path, cfg))

	rt := runtime.New(runtime.WithInstanceID("noncas-test"))
	apiCfg := testConfig()
	apiCfg.ConfigStore = &parser.FileStore{Path: path, Registry: newTestRegistry(t)}
	apiCfg.ConfigProvider = func() *ports.BridgeConfig { return cfg }
	// Fail-closed default: ConfigSingleWriter is intentionally left false to
	// model a cluster-capable deployment over a shared non-CAS file store.
	apiCfg.ConfigSingleWriter = false

	s := New(rt, apiCfg)

	txnID := createTxn(t, s)
	applyPatch(t, s, txnID, `{"bridge": {"id": "test-bridge", "log_level": "error"}}`)

	rec := httptest.NewRecorder()
	req := adminRequest(http.MethodPost, "/api/v1/admin/config/transactions/"+txnID+"/commit")
	req.SetPathValue("txnID", txnID)
	s.handleConfigTxnCommit(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code, "non-CAS commit must fail closed, body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "compare-and-swap")

	// The file must be untouched: still the original version, and the patched
	// log_level ("error") must NOT have been persisted (the fixture leaves
	// log_level unset, so any non-empty value proves a leaked write).
	parsed, err := parser.ParseFile(path, parser.FormatYAML, newTestRegistry(t))
	require.NoError(t, err)
	assert.Equal(t, cfg.Version, parsed.Version, "refused commit must not bump the on-disk version")
	assert.NotEqual(t, "error", parsed.Bridge.LogLevel, "refused commit must not persist the patched log_level")
}
