package httpapi

// Residual regression tests for the "HTTP admin/monitor API" chunk of the
// production-readiness audit. Each test pins a fix that existed in the code
// but had no test observing it at the exact seam the audit called out:
//
//   - Commit-path wiring of guardNoConfigLoss (the CRITICAL options-loss
//     class): the guard function was unit-tested in isolation, but disabling
//     its call site inside configTxnManager.Commit left the suite green.
//   - POST /dlq/purge refusing to run without confirm_purge_all=true (only
//     the confirmed path was tested).
//   - GET /dlq/messages/{id} emitting the dlq.read_payload audit event.
//   - GET /dlq/messages rejecting offset > maxDLQOffset.
//   - GET /dlq flagging count_capped when the summary scan hits its cap.
//   - Start deriving the admin WriteTimeout as AdminOperationTimeout plus
//     adminWriteTimeoutMargin so a slow-but-successful admin operation can
//     flush its response before the write deadline fires.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// guardTestStore is a minimal in-memory ports.ConfigStore whose Load returns
// a fixed on-disk snapshot and whose Save records invocations. It lets the
// Commit-path options-loss guard be exercised without a real file store,
// whose parser would re-decode plugin options and mask the loss scenario.
type guardTestStore struct {
	disk      *ports.BridgeConfig
	saveCalls int
}

func (s *guardTestStore) Load(_ context.Context) (*ports.BridgeConfig, error) {
	clone := *s.disk
	return &clone, nil
}

func (s *guardTestStore) Save(_ context.Context, _ *ports.BridgeConfig) error {
	s.saveCalls++
	return nil
}

func (s *guardTestStore) Validate(_ context.Context, _ *ports.BridgeConfig) ([]string, error) {
	return nil, nil
}

func (s *guardTestStore) Merge(_ context.Context, _, overlay *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	clone := *overlay
	return &clone, nil
}

var _ ports.ConfigStore = (*guardTestStore)(nil)

// TestConfigTxnCommit_RefusesOptionsLoss_AndDoesNotSave pins the COMMIT-PATH
// wiring of guardNoConfigLoss: when the merged config (based on disk) lost a
// typed plugin Config that the running config still carries, Commit must fail
// with errConfigOptionsLoss BEFORE any Save — persisting would erase the
// entry's broker URL/credentials from disk permanently.
func TestConfigTxnCommit_RefusesOptionsLoss_AndDoesNotSave(t *testing.T) {
	// Running config: sess-1 carries a decoded plugin Config.
	memCfg := sampleBridgeConfig()
	memCfg.Sessions[0].SetDecoded(stubPluginConfig{kind: "mqtt"}, nil)

	// On-disk snapshot: same entry WITHOUT its plugin Config (the corruption
	// the guard exists to catch — e.g. a hand-edit or an earlier bad write).
	diskCfg := sampleBridgeConfig()
	diskCfg.Version = 1

	store := &guardTestStore{disk: diskCfg}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	mgr := newTxnManager(store, func() *ports.BridgeConfig { return memCfg }, nil, nil, clk)

	ctx := context.Background()
	txn, err := mgr.Begin(ctx, 0)
	require.NoError(t, err)
	defer func() { _ = mgr.Rollback(txn.ID) }()

	_, err = mgr.Commit(ctx, txn.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, errConfigOptionsLoss)
	assert.Equal(t, 0, store.saveCalls,
		"a commit refused by the options-loss guard must never reach Save")
}

// TestHandleDLQPurge_RequiresConfirmation pins the destructive-operation
// guard: purging the ENTIRE DLQ without confirm_purge_all=true must be
// refused with 400 and must not touch the store. Mirrors the
// confirm_delete_all guard on delete-by-filter.
func TestHandleDLQPurge_RequiresConfirmation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "empty object", body: `{}`},
		{name: "explicit false", body: `{"confirm_purge_all":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			purgeCalls := 0
			store := &mockDLQStore{
				purgeFunc: func(_ context.Context, _ time.Time) (int, error) {
					purgeCalls++
					return 0, nil
				},
			}
			_, mux := dlqServer(store)

			rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/purge", tc.body))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "confirm_purge_all")
			assert.Equal(t, 0, purgeCalls,
				"an unconfirmed purge must never reach the store")
		})
	}
}

// TestHandleDLQMessageByID_AuditsPayloadRead pins the dlq.read_payload audit
// event: a single-entry read returns the full (base64) payload, which can
// carry PII/secrets, so the disclosure must be attributable — exactly like
// config reads.
func TestHandleDLQMessageByID_AuditsPayloadRead(t *testing.T) {
	entry := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "msg-audit-1", RouteID: "r1",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: "test/sub",
			Payload: []byte("sensitive"),
		}),
	})
	store := &mockDLQStore{
		getFunc: func(_ context.Context, id string) (routing.DLQEntry, error) {
			return entry, nil
		},
	}
	rt := runtime.New(runtime.WithInstanceID("dlq-audit-test"), runtime.WithDLQStore(store))
	audit := &recordingAuditLogger{}
	s := New(rt, testConfig(), WithAuditLogger(audit))
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	rec := dlqDo(mux, dlqReq("GET", "/api/v1/admin/dlq/messages/msg-audit-1", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	var found bool
	for _, ev := range audit.Events() {
		if ev.Action == "dlq.read_payload" {
			found = true
			assert.Equal(t, "dlq", ev.Resource)
			assert.Equal(t, "msg-audit-1", ev.ResourceID)
			assert.Equal(t, "success", ev.Outcome)
		}
	}
	assert.True(t, found, "payload read must emit a dlq.read_payload audit event")
}

// TestHandleDLQMessages_OffsetAboveMaxRejected pins the pagination bound: an
// offset beyond maxDLQOffset must be rejected with 400 rather than forcing
// the store to materialize an unbounded prefix (or overflowing offset+limit).
func TestHandleDLQMessages_OffsetAboveMaxRejected(t *testing.T) {
	listCalls := 0
	store := &mockDLQStore{
		listFunc: func(_ context.Context, _ routing.DLQFilter) ([]routing.DLQEntry, error) {
			listCalls++
			return nil, nil
		},
	}
	_, mux := dlqServer(store)

	rec := dlqDo(mux, dlqReq("GET",
		fmt.Sprintf("/api/v1/admin/dlq/messages?offset=%d", maxDLQOffset+1), ""))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, listCalls, "an over-limit offset must be rejected before the store is queried")

	// The boundary value itself is accepted.
	rec = dlqDo(mux, dlqReq("GET",
		fmt.Sprintf("/api/v1/admin/dlq/messages?offset=%d", maxDLQOffset), ""))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestHandleDLQ_SummaryCountCapped pins the /dlq summary honesty flag: when
// the backlog reaches the summary scan cap, count_capped must be true so an
// operator alerting on count knows the true depth is AT LEAST that large.
func TestHandleDLQ_SummaryCountCapped(t *testing.T) {
	mkEntries := func(n int) []routing.DLQEntry {
		entries := make([]routing.DLQEntry, n)
		for i := range entries {
			entries[i] = routing.RehydrateDLQEntry(routing.DLQEntrySpec{
				ID:       fmt.Sprintf("cap-%d", i),
				FailedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			})
		}
		return entries
	}

	t.Run("below cap", func(t *testing.T) {
		store := &mockDLQStore{
			listFunc: func(_ context.Context, f routing.DLQFilter) ([]routing.DLQEntry, error) {
				return mkEntries(3), nil
			},
		}
		_, mux := dlqServer(store)
		rec := dlqDo(mux, dlqReq("GET", "/api/v1/admin/dlq", ""))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `"count":3`)
		assert.Contains(t, rec.Body.String(), `"count_capped":false`)
	})

	t.Run("at cap", func(t *testing.T) {
		store := &mockDLQStore{
			listFunc: func(_ context.Context, f routing.DLQFilter) ([]routing.DLQEntry, error) {
				if f.Limit > 0 && f.Limit < dlqSummaryCap {
					return mkEntries(f.Limit), nil
				}
				return mkEntries(dlqSummaryCap), nil
			},
		}
		_, mux := dlqServer(store)
		rec := dlqDo(mux, dlqReq("GET", "/api/v1/admin/dlq", ""))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `"count_capped":true`)
	})
}

// TestHandleHealth_LeaksNothingBeyondStatus pins the unauthenticated /health
// response shape: exactly one key ("status"). instance_id, route counts, and
// component-failure names are operational reconnaissance and belong behind
// auth on /deephealth.
func TestHandleHealth_LeaksNothingBeyondStatus(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("health-shape-test"))
	s := New(rt, testConfig())

	rec := dlqDo(s.MonitorMux(), dlqReq("GET", "/api/v1/monitor/health", ""))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Len(t, body, 1, "unauth /health must expose only the coarse status")
	assert.Contains(t, body, "status")
}

// TestStart_AdminWriteTimeoutExceedsOperationTimeout pins the timeout
// relationship: the admin server's WriteTimeout must be AdminOperationTimeout
// plus a positive margin, so an operation that legitimately runs up to its
// budget can still flush its response instead of having the connection reset
// mid-write (leaving the operator retrying against ambiguous state).
func TestStart_AdminWriteTimeoutExceedsOperationTimeout(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("write-timeout-test"))
	cfg := testConfig()
	cfg.AdminOperationTimeout = 5 * time.Second
	s := New(rt, cfg)

	require.NoError(t, s.Start(context.Background()))
	defer func() { _ = s.Stop(context.Background()) }()

	require.NotNil(t, s.admin)
	assert.Equal(t, cfg.AdminOperationTimeout+adminWriteTimeoutMargin, s.admin.WriteTimeout)
	assert.Greater(t, s.admin.WriteTimeout, cfg.AdminOperationTimeout,
		"admin WriteTimeout must strictly exceed AdminOperationTimeout")
}
