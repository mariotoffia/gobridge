package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/store/memorydlq"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// refusingSender fails every send with a fixed error, modelling the downstream
// that was still refusing when the operator redrove the entry.
type refusingSender struct{ err error }

func (s *refusingSender) Send(context.Context, ports.OutboundMessage) error { return s.err }

// dropRouteRedriveSetup wires the admin API to a real runtime whose only route
// DISCARDS a failed delivery (on_permanent_failure=drop), plus a real in-memory
// DLQ store. This is the shape from the issue: a redrive that fails is dropped,
// so the DLQ entry is the message's only remaining copy.
func dropRouteRedriveSetup(t *testing.T, sendErr error) (*http.ServeMux, *memorydlq.Store) {
	t.Helper()
	store := memorydlq.NewStore()
	recv := newStubReceiver()
	rt := runtime.New(
		runtime.WithInstanceID("redrive-evidence-test"),
		runtime.WithDLQStore(store),
	)
	cfg := runtime.RouteConfig{
		ID: "drop-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			OnPermanentFailure: routing.FailureDrop,
			MaxReplayAttempts:  3,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	require.NoError(t, rt.AddRoute(cfg, recv, &refusingSender{err: sendErr}, nil, nil))
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	<-recv.ready

	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)
	return mux, store
}

// TestHandleDLQRedrive_DroppedRedrive_KeepsEntry pins the evidence rule: when a
// redrive is not delivered — here the route DROPS it under
// on_permanent_failure=drop — the redrive is reported as a failure and the
// original DLQ entry is left in place. Deleting it would destroy the message and
// the only record that it ever failed.
//
// Mutation check: report the dropped redrive as a successful inject and this
// fails — the entry is deleted and the response says "redriven".
func TestHandleDLQRedrive_DroppedRedrive_KeepsEntry(t *testing.T) {
	mux, store := dropRouteRedriveSetup(t, shared.ErrInvalidPayload)
	seedDLQ(t, store, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "keep-1", RouteID: "drop-route",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID: "orig-keep-1", Subject: "s1", Payload: []byte("p1"),
		}),
		FailedAt: time.Now(),
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["keep-1"]}`))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, http.StatusMultiStatus, rec.Code,
		"a dropped redrive is not a success")
	assert.Equal(t, float64(0), body["redriven"])
	assert.Equal(t, float64(1), body["failed"])

	remaining, err := store.List(context.Background(), routing.DLQFilter{})
	require.NoError(t, err)
	require.Len(t, remaining, 1, "the DLQ entry for a dropped redrive must be preserved")
	assert.Equal(t, "keep-1", remaining[0].ID())
}

// TestHandleDLQRedrive_ReDLQdRedrive_KeepsEntry pins the same rule for the
// DEFAULT retention policy: a redrive that fails and lands back in the DLQ has
// not been delivered either, so it is reported as a failure and the original
// entry stays. The route writes a fresh entry for the new failure, so the
// operator sees both and can delete the stale one deliberately.
//
// Mutation check: report the re-DLQ'd redrive as a successful inject and this
// fails — the original entry is deleted although nothing was delivered.
func TestHandleDLQRedrive_ReDLQdRedrive_KeepsEntry(t *testing.T) {
	store := memorydlq.NewStore()
	recv := newStubReceiver()
	rt := runtime.New(
		runtime.WithInstanceID("redrive-redlq-test"),
		runtime.WithDLQStore(store),
	)
	cfg := runtime.RouteConfig{
		ID: "dlq-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			OnPermanentFailure: routing.FailureDLQ,
			MaxReplayAttempts:  3,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	require.NoError(t, rt.AddRoute(cfg, recv, &refusingSender{err: shared.ErrInvalidPayload}, nil, nil))
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	<-recv.ready

	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	seedDLQ(t, store, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "redlq-1", RouteID: "dlq-route",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID: "orig-redlq-1", Subject: "s1", Payload: []byte("p1"),
		}),
		FailedAt: time.Now(),
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["redlq-1"]}`))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, http.StatusMultiStatus, rec.Code,
		"a redrive that lands back in the DLQ is not a success")
	assert.Equal(t, float64(0), body["redriven"])

	remaining, err := store.List(context.Background(), routing.DLQFilter{})
	require.NoError(t, err)
	var found bool
	for _, e := range remaining {
		if e.ID() == "redlq-1" {
			found = true
		}
	}
	assert.True(t, found, "the original DLQ entry must survive a re-DLQ'd redrive")
}

// TestHandleInject_DroppedByPolicy_ReportsNotDelivered pins how the ordinary
// message-inject endpoint reports a route that did NOT deliver the message. A
// route that drops, filters or expires a message is behaving as configured, so
// the answer is neither "injected" (a lie about delivery) nor 500 (a lie about
// a server defect): it is a distinct non-2xx outcome naming the reason, so an
// operator testing a route sees what the route actually did.
//
// Mutation check: map the non-delivery to 500 (or to 200 "injected") and this
// fails.
func TestHandleInject_DroppedByPolicy_ReportsNotDelivered(t *testing.T) {
	mux, _ := dropRouteRedriveSetup(t, shared.ErrInvalidPayload)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/routes/drop-route/inject",
		strings.NewReader(`{"subject":"s","payload":"aGk="}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotEmpty(t, body["error"], "the response must name why the message was not delivered")
}
