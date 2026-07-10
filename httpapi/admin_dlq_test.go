package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════
// Mock DLQ Store
//
// Configurable per-method via function fields. Each method delegates
// to its function if non-nil, otherwise returns a safe default.
// ═══════════════════════════════════════════════════════════════════

type mockDLQStore struct {
	getFunc          func(ctx context.Context, id string) (routing.DLQEntry, error)
	listFunc         func(ctx context.Context, f routing.DLQFilter) ([]routing.DLQEntry, error)
	deleteFunc       func(ctx context.Context, ids []string) (int, error)
	deleteByFilterFn func(ctx context.Context, f routing.DLQFilter) (int, error)
	writeFunc        func(ctx context.Context, e routing.DLQEntry) error
	purgeFunc        func(ctx context.Context, before time.Time) (int, error)
}

func (m *mockDLQStore) Get(ctx context.Context, id string) (routing.DLQEntry, error) {
	if m.getFunc != nil {
		return m.getFunc(ctx, id)
	}
	return routing.DLQEntry{}, shared.ErrNotFound
}
func (m *mockDLQStore) List(ctx context.Context, f routing.DLQFilter) ([]routing.DLQEntry, error) {
	if m.listFunc != nil {
		return m.listFunc(ctx, f)
	}
	return nil, nil
}
func (m *mockDLQStore) Delete(ctx context.Context, ids []string) (int, error) {
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, ids)
	}
	return len(ids), nil
}
func (m *mockDLQStore) DeleteByFilter(ctx context.Context, f routing.DLQFilter) (int, error) {
	if m.deleteByFilterFn != nil {
		return m.deleteByFilterFn(ctx, f)
	}
	return 0, nil
}
func (m *mockDLQStore) Write(ctx context.Context, e routing.DLQEntry) error {
	if m.writeFunc != nil {
		return m.writeFunc(ctx, e)
	}
	return nil
}
func (m *mockDLQStore) Purge(ctx context.Context, before time.Time) (int, error) {
	if m.purgeFunc != nil {
		return m.purgeFunc(ctx, before)
	}
	return 0, nil
}

func dlqServer(store *mockDLQStore) (*Server, *http.ServeMux) {
	rt := runtime.New(runtime.WithInstanceID("dlq-test"), runtime.WithDLQStore(store))
	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)
	return s, mux
}

func dlqReq(method, path, body string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("X-API-Key", "test-secret-key-0123456789")
	return r
}

func dlqDo(mux *http.ServeMux, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ═══════════════════════════════════════════════════════════════════
// View conversion tests
// ═══════════════════════════════════════════════════════════════════

// TestToDLQEntryView_MapsAllFields validates that toDLQEntryView
// correctly maps all domain fields including Subject from Envelope.
func TestToDLQEntryView_MapsAllFields(t *testing.T) {
	e := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "v-1", RouteID: "r1", BindingID: "b1", SessionID: "s1",
		SourceID: "src1", CorrelationID: "c1", Reason: "timeout",
		Category: "transient", ErrorCode: "TIMEOUT", LastError: "dial err",
		FailedAt: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC), Attempts: 5,
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "test/topic"}),
	})
	v := toDLQEntryView(e)
	assert.Equal(t, "v-1", v.ID)
	assert.Equal(t, "r1", v.RouteID)
	assert.Equal(t, "test/topic", v.Subject)
	assert.Equal(t, "transient", v.Category)
	assert.Equal(t, 5, v.Attempts)
}

// TestToDLQEntryDetailView_IncludesBase64Payload validates that the
// detail view includes the envelope payload as base64.
func TestToDLQEntryDetailView_IncludesBase64Payload(t *testing.T) {
	e := routing.NewDLQEntry(routing.DLQEntrySpec{
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"msg":"hello"}`)}),
	})
	v := toDLQEntryDetailView(e)
	decoded, err := base64.StdEncoding.DecodeString(v.Payload)
	require.NoError(t, err)
	assert.JSONEq(t, `{"msg":"hello"}`, string(decoded))
}

// TestToDLQEntryDetailView_EmptyPayload validates empty payload produces
// empty base64 string.
func TestToDLQEntryDetailView_EmptyPayload(t *testing.T) {
	v := toDLQEntryDetailView(routing.DLQEntry{})
	assert.Equal(t, "", v.Payload)
}

// ═══════════════════════════════════════════════════════════════════
// GET /dlq/messages/{id}
// ═══════════════════════════════════════════════════════════════════

// TestHandleDLQMessageByID_ReturnsEntry validates a successful get-by-id
// returns 200 with all dlqEntryDetailView fields including base64 payload.
func TestHandleDLQMessageByID_ReturnsEntry(t *testing.T) {
	entry := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "msg-1", RouteID: "r1", Category: "timeout",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: "test/sub",
			Payload: []byte("binary-data"),
		}),
		FailedAt: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
	})
	store := &mockDLQStore{
		getFunc: func(_ context.Context, id string) (routing.DLQEntry, error) {
			if id == "msg-1" {
				return entry, nil
			}
			return routing.DLQEntry{}, shared.ErrNotFound
		},
	}
	_, mux := dlqServer(store)

	rec := dlqDo(mux, dlqReq("GET", "/api/v1/admin/dlq/messages/msg-1", ""))
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "msg-1", body["id"])
	assert.Equal(t, "r1", body["route_id"])
	assert.Equal(t, "test/sub", body["subject"])
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("binary-data")), body["payload"])
}

// TestHandleDLQMessageByID_NotFound validates 404 for missing entry.
func TestHandleDLQMessageByID_NotFound(t *testing.T) {
	_, mux := dlqServer(&mockDLQStore{})
	rec := dlqDo(mux, dlqReq("GET", "/api/v1/admin/dlq/messages/nonexistent", ""))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestHandleDLQMessageByID_StoreError validates 500 for non-ErrNotFound errors.
func TestHandleDLQMessageByID_StoreError(t *testing.T) {
	store := &mockDLQStore{
		getFunc: func(_ context.Context, _ string) (routing.DLQEntry, error) {
			return routing.DLQEntry{}, fmt.Errorf("connection lost")
		},
	}
	_, mux := dlqServer(store)
	rec := dlqDo(mux, dlqReq("GET", "/api/v1/admin/dlq/messages/any", ""))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ═══════════════════════════════════════════════════════════════════
// POST /dlq/delete
// ═══════════════════════════════════════════════════════════════════

// TestHandleDLQDeleteByIDs_Success validates successful deletion returns
// the count of deleted entries.
func TestHandleDLQDeleteByIDs_Success(t *testing.T) {
	store := &mockDLQStore{
		deleteFunc: func(_ context.Context, ids []string) (int, error) {
			return len(ids), nil
		},
	}
	_, mux := dlqServer(store)

	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete", `{"ids":["a","b"]}`))
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]int
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 2, body["deleted"])
}

// TestHandleDLQDeleteByIDs_EmptyIDs validates 400 for empty id array.
func TestHandleDLQDeleteByIDs_EmptyIDs(t *testing.T) {
	_, mux := dlqServer(&mockDLQStore{})
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete", `{"ids":[]}`))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleDLQDeleteByIDs_ExceedsMax validates 400 when exceeding 1000 IDs.
func TestHandleDLQDeleteByIDs_ExceedsMax(t *testing.T) {
	ids := make([]string, 1001)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%d", i)
	}
	body, _ := json.Marshal(map[string][]string{"ids": ids})
	_, mux := dlqServer(&mockDLQStore{})
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete", string(body)))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleDLQDeleteByIDs_InvalidBody validates 400 for non-JSON body.
func TestHandleDLQDeleteByIDs_InvalidBody(t *testing.T) {
	_, mux := dlqServer(&mockDLQStore{})
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete", "not-json"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleDLQDeleteByIDs_StoreError validates 500 when store fails.
func TestHandleDLQDeleteByIDs_StoreError(t *testing.T) {
	store := &mockDLQStore{
		deleteFunc: func(_ context.Context, _ []string) (int, error) {
			return 0, fmt.Errorf("db down")
		},
	}
	_, mux := dlqServer(store)
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete", `{"ids":["a"]}`))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ═══════════════════════════════════════════════════════════════════
// POST /dlq/delete-by-filter
// ═══════════════════════════════════════════════════════════════════

// TestHandleDLQDeleteByFilter_ByRouteID validates filtering by route_id.
func TestHandleDLQDeleteByFilter_ByRouteID(t *testing.T) {
	var captured routing.DLQFilter
	store := &mockDLQStore{
		deleteByFilterFn: func(_ context.Context, f routing.DLQFilter) (int, error) {
			captured = f
			return 3, nil
		},
	}
	_, mux := dlqServer(store)
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter",
		`{"route_id":"r1"}`))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "r1", captured.RouteID)

	var body map[string]int
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 3, body["deleted"])
}

// TestHandleDLQDeleteByFilter_EmptyRequiresConfirm validates the safety
// guard: empty filter without confirm_delete_all returns 400.
func TestHandleDLQDeleteByFilter_EmptyRequiresConfirm(t *testing.T) {
	_, mux := dlqServer(&mockDLQStore{})
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter", `{}`))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "confirm_delete_all")
}

// TestHandleDLQDeleteByFilter_EmptyWithConfirm validates that
// confirm_delete_all=true allows unfiltered delete.
func TestHandleDLQDeleteByFilter_EmptyWithConfirm(t *testing.T) {
	store := &mockDLQStore{
		deleteByFilterFn: func(_ context.Context, _ routing.DLQFilter) (int, error) {
			return 42, nil
		},
	}
	_, mux := dlqServer(store)
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter",
		`{"confirm_delete_all":true}`))
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]int
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 42, body["deleted"])
}

// TestHandleDLQDeleteByFilter_InvalidSince validates 400 for bad timestamp.
func TestHandleDLQDeleteByFilter_InvalidSince(t *testing.T) {
	_, mux := dlqServer(&mockDLQStore{})
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter",
		`{"since":"not-a-date"}`))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleDLQDeleteByFilter_InvalidBefore validates 400 for bad timestamp.
func TestHandleDLQDeleteByFilter_InvalidBefore(t *testing.T) {
	_, mux := dlqServer(&mockDLQStore{})
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter",
		`{"before":"not-a-date"}`))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleDLQDeleteByFilter_StoreError validates 500 on store failure.
func TestHandleDLQDeleteByFilter_StoreError(t *testing.T) {
	store := &mockDLQStore{
		deleteByFilterFn: func(_ context.Context, _ routing.DLQFilter) (int, error) {
			return 0, fmt.Errorf("db error")
		},
	}
	_, mux := dlqServer(store)
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter",
		`{"route_id":"r1"}`))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestHandleDLQDeleteByFilter_NegativeLimit_RejectedNoDelete pins [HIGH-2]: a
// negative limit must be rejected with 400 BEFORE any DeleteByFilter call. The
// DeleteByFilter port contract treats Limit <= 0 as "delete EVERY matching
// entry", so copying a negative limit into the filter would silently turn a
// bounded-looking request into an UNBOUNDED destructive delete.
//
// Mutation reasoning — remove the `if body.Limit < 0` guard and this test
// fails: the handler builds a filter with Limit -1, calls DeleteByFilter (the
// spy records the call), and returns 200 instead of 400.
func TestHandleDLQDeleteByFilter_NegativeLimit_RejectedNoDelete(t *testing.T) {
	var calls int
	store := &mockDLQStore{
		deleteByFilterFn: func(_ context.Context, _ routing.DLQFilter) (int, error) {
			calls++
			return 999, nil
		},
	}
	_, mux := dlqServer(store)
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter",
		`{"route_id":"prod-route","limit":-1}`))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "limit must not be negative")
	assert.Equal(t, 0, calls, "a negative limit must never reach the store's DeleteByFilter")
}

// TestHandleDLQDeleteByFilter_HugeLimitOnly_RejectedNoDelete pins [FINDING 1]:
// a bare huge positive limit with no other predicate is BOTH an unconfirmed
// delete-all AND an unbounded destructive scan. It must be rejected with 400 by
// the max-limit cap BEFORE any DeleteByFilter call — a limit is a bound, not a
// filter, so it can never bypass the confirm_delete_all guard nor launch a
// 2.1-billion-row delete.
//
// Mutation reasoning — (a) restoring `|| filter.Limit > 0` in hasFilter would
// make this bare-limit request "filtered" and skip confirm; (b) removing the
// maxDeleteByFilterLimit cap would let it through to the store. Either regression
// makes this test fail (DeleteByFilter called, 200 instead of 400).
func TestHandleDLQDeleteByFilter_HugeLimitOnly_RejectedNoDelete(t *testing.T) {
	var calls int
	store := &mockDLQStore{
		deleteByFilterFn: func(_ context.Context, _ routing.DLQFilter) (int, error) {
			calls++
			return 2147483647, nil
		},
	}
	_, mux := dlqServer(store)
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter",
		`{"limit":2147483647}`))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "limit exceeds maximum")
	assert.Equal(t, 0, calls, "a huge limit must never reach the store's DeleteByFilter")
}

// TestHandleDLQDeleteByFilter_BareLimit_RequiresConfirm pins the other half of
// [FINDING 1] fix (a): a modest bare limit (within the cap) with NO route/
// category/time predicate is still an unconfirmed delete-all and must be gated
// by confirm_delete_all — a positive limit alone no longer satisfies hasFilter.
func TestHandleDLQDeleteByFilter_BareLimit_RequiresConfirm(t *testing.T) {
	var calls int
	store := &mockDLQStore{
		deleteByFilterFn: func(_ context.Context, _ routing.DLQFilter) (int, error) {
			calls++
			return 10, nil
		},
	}
	_, mux := dlqServer(store)
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter",
		`{"limit":10}`))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "confirm_delete_all")
	assert.Equal(t, 0, calls, "a bare limit must not bypass the delete-all confirmation")
}

// TestHandleDLQDeleteByFilter_PositiveLimit_BoundsDelete confirms a valid
// positive limit WITH a real filter is still honoured and passed through to the
// store (a bounded delete on route X must keep working).
func TestHandleDLQDeleteByFilter_PositiveLimit_BoundsDelete(t *testing.T) {
	var captured routing.DLQFilter
	store := &mockDLQStore{
		deleteByFilterFn: func(_ context.Context, f routing.DLQFilter) (int, error) {
			captured = f
			return 5, nil
		},
	}
	_, mux := dlqServer(store)
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter",
		`{"route_id":"prod-route","limit":5}`))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 5, captured.Limit)
	var body map[string]int
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 5, body["deleted"])
}

// TestHandleDLQDeleteByFilter_ZeroLimit_UnboundedWithinFilter documents the
// deliberate [HIGH-2] decision: limit == 0 (the omitted-field default) stays
// "unbounded within the provided filter" per the port contract — here, delete
// all entries for the given route. The hasFilter guard keeps a limit==0 request
// with NO other filter unambiguous (it still demands confirm_delete_all).
func TestHandleDLQDeleteByFilter_ZeroLimit_UnboundedWithinFilter(t *testing.T) {
	var captured routing.DLQFilter
	var calls int
	store := &mockDLQStore{
		deleteByFilterFn: func(_ context.Context, f routing.DLQFilter) (int, error) {
			calls++
			captured = f
			return 7, nil
		},
	}
	_, mux := dlqServer(store)
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter",
		`{"route_id":"prod-route","limit":0}`))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, calls, "limit==0 with a route filter is a legitimate bounded-by-filter delete")
	assert.Equal(t, 0, captured.Limit)
	assert.Equal(t, "prod-route", captured.RouteID)
}

// TestHandleDLQDeleteByFilter_WithTimeRange validates Since and Before
// are correctly parsed and passed to the store.
func TestHandleDLQDeleteByFilter_WithTimeRange(t *testing.T) {
	var captured routing.DLQFilter
	store := &mockDLQStore{
		deleteByFilterFn: func(_ context.Context, f routing.DLQFilter) (int, error) {
			captured = f
			return 1, nil
		},
	}
	_, mux := dlqServer(store)
	rec := dlqDo(mux, dlqReq("POST", "/api/v1/admin/dlq/delete-by-filter",
		`{"route_id":"r1","since":"2024-01-01T00:00:00Z","before":"2024-06-01T00:00:00Z"}`))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "r1", captured.RouteID)
	assert.False(t, captured.Since.IsZero())
	assert.False(t, captured.Before.IsZero())
}

// ═══════════════════════════════════════════════════════════════════
// GET /dlq/messages (pagination)
// ═══════════════════════════════════════════════════════════════════

// TestHandleDLQMessages_Pagination validates offset/limit are applied and
// has_more reflects that further pages exist (there is no truthful total
// without a Count port; has_more replaces the old lying total field).
func TestHandleDLQMessages_Pagination(t *testing.T) {
	entries := make([]routing.DLQEntry, 10)
	for i := range entries {
		entries[i] = routing.RehydrateDLQEntry(routing.DLQEntrySpec{
			ID:       fmt.Sprintf("p-%d", i),
			FailedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour),
		})
	}
	store := &mockDLQStore{
		listFunc: func(_ context.Context, _ routing.DLQFilter) ([]routing.DLQEntry, error) {
			return entries, nil
		},
	}
	_, mux := dlqServer(store)

	rec := dlqDo(mux, dlqReq("GET", "/api/v1/admin/dlq/messages?limit=3&offset=2", ""))
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, true, body["has_more"])
	assert.Equal(t, float64(3), body["limit"])
	assert.Equal(t, float64(2), body["offset"])
	msgs := body["messages"].([]any)
	assert.Len(t, msgs, 3)
}

// TestHandleDLQMessages_InvalidLimit validates 400 for non-integer limit.
func TestHandleDLQMessages_InvalidLimit(t *testing.T) {
	_, mux := dlqServer(&mockDLQStore{})
	rec := dlqDo(mux, dlqReq("GET", "/api/v1/admin/dlq/messages?limit=abc", ""))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleDLQMessages_InvalidOffset validates 400 for negative offset.
func TestHandleDLQMessages_InvalidOffset(t *testing.T) {
	_, mux := dlqServer(&mockDLQStore{})
	rec := dlqDo(mux, dlqReq("GET", "/api/v1/admin/dlq/messages?offset=-1", ""))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
