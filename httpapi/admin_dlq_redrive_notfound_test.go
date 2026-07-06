package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// redriveBoundSetup builds a redrive server whose route carries a single real
// binding ("b1"). It mirrors redriveSetup but with a live binding so a redrive
// that references a DIFFERENT (renamed/removed) binding can be shown to either
// (a) be rejected as not-found — the fix — or (b) silently fan out to the
// healthy current binding — the pre-fix bug.
func redriveBoundSetup(t *testing.T) (*http.ServeMux, *memorydlq.Store, *stubSender) {
	t.Helper()
	dlq := memorydlq.NewStore()
	sender := newStubSender()
	recv := newStubReceiver()
	rt := runtime.New(
		runtime.WithInstanceID("redrive-notfound-test"),
		runtime.WithDLQStore(dlq),
	)
	cfg := runtime.RouteConfig{
		ID: "test-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
		// The route's ONLY current binding. A redrive recorded against any
		// other binding ID must not be delivered here.
		Bindings: []routing.DestinationBinding{{ID: "b1", Address: "addr-b1"}},
	}
	require.NoError(t, rt.AddRoute(cfg, recv, sender, nil, nil))
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	<-recv.ready

	s := New(rt, testConfig())
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)
	return mux, dlq, sender
}

// TestHandleDLQRedrive_UnknownBinding_RejectsAndPreservesEntry is the finding-1
// out-of-band-confinement guard at the HTTP redrive boundary. An operator
// redrives a DLQ entry whose recorded BindingID ("ghost-binding") no longer
// exists on the route (it was renamed/removed to "b1"). The redrive threads that
// binding out-of-band into Runtime.InjectRedrive → the route runner, which must
// reject an unknown OUT-OF-BAND binding as a PERMANENT shared.ErrNotFound BEFORE
// the message enters the pipeline — never falling through to normal resolution
// and re-delivering to the healthy current binding. Because the inject fails,
// the admin redrive handler restores the claimed DLQ entry, so failure evidence
// is preserved and nothing is dispatched.
//
// Fails without the fix (neutralise the out-of-band binding validation in
// runner.doHandleDelivery): the unknown binding falls through to normal
// resolution, the replay is delivered to binding "b1" (sender called once), the
// inject reports success, and the DLQ entry is deleted — the silent fan-out the
// finding describes.
func TestHandleDLQRedrive_UnknownBinding_RejectsAndPreservesEntry(t *testing.T) {
	mux, dlq, sender := redriveBoundSetup(t)
	seedDLQ(t, dlq, routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:      "e1",
		RouteID: "test-route",
		// The binding that failed and was recorded no longer exists on the
		// route (the current binding is "b1").
		BindingID: "ghost-binding",
		Envelope:  *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "orig-1", Subject: "s1", Payload: []byte("p1")}),
		FailedAt:  time.Now(),
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, redriveReq(`{"ids":["e1"]}`))
	require.Equal(t, http.StatusMultiStatus, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(0), body["redriven"], "an unknown-binding redrive must inject to NO binding")
	assert.Equal(t, float64(1), body["failed"])

	errs, ok := body["errors"].([]any)
	require.True(t, ok, "expected an errors array for the rejected redrive")
	require.Len(t, errs, 1)
	errObj := errs[0].(map[string]any)
	assert.Equal(t, "e1", errObj["id"])
	assert.Contains(t, errObj["error"], "not found")

	// Nothing was dispatched: the replay never reached the healthy binding.
	assert.Equal(t, 0, sender.sentCount(), "unknown-binding redrive must not fan out to the current binding")

	// The claimed entry is restored (best-effort) so failure evidence survives.
	remaining, _ := dlq.List(context.Background(), routing.DLQFilter{})
	assert.Len(t, remaining, 1, "a rejected redrive must preserve the DLQ entry")
}
