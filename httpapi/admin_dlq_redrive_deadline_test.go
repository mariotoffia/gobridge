package httpapi

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// slowSubject marks the envelope whose destination never answers.
const slowSubject = "slow"

// subjectBlockingSender stands in for a destination that is down for one
// subject: a send of slowSubject blocks until its context ends, any other
// subject is delivered at once.
type subjectBlockingSender struct{}

func (subjectBlockingSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	if msg.Envelope.Subject() == slowSubject {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// redriveResponse is the part of a redrive response these tests assert on.
type redriveResponse struct {
	Redriven int `json:"redriven"`
	Errors   []struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	} `json:"errors"`
}

func decodeRedriveResponse(t *testing.T, rec *httptest.ResponseRecorder) redriveResponse {
	t.Helper()
	var body redriveResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// slowRedriveServer is redriveSetup with a destination that blocks on
// slowSubject; it returns the *Server so a test can shrink its redrive bounds.
func slowRedriveServer(t *testing.T) (*Server, *http.ServeMux, *memorydlq.Store) {
	t.Helper()
	dlq := memorydlq.NewStore()
	recv := newStubReceiver()
	rt := runtime.New(
		runtime.WithInstanceID("redrive-deadline-test"),
		runtime.WithDLQStore(dlq),
	)
	cfg := runtime.RouteConfig{
		ID: "test-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}
	require.NoError(t, rt.AddRoute(cfg, recv, subjectBlockingSender{}, nil, nil))
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	wait.RequireClosed(t, recv.ready, 2*time.Second)

	srv := New(rt, testConfig())
	mux := http.NewServeMux()
	srv.registerAdminRoutes(mux)
	return srv, mux, dlq
}

// An entry whose destination is down may spend only its own bound, not the
// whole batch: the ids after it are still looked up, injected and removed.
func TestRedriveASlowFirstEntryDoesNotStarveTheBatch(t *testing.T) {
	srv, mux, dlq := slowRedriveServer(t)
	// Each fast entry must finish inside this real bound under -race on a
	// loaded machine, so it is generous; the batch bound stays well above
	// the slow entry's whole bound plus the fast ones.
	srv.redriveEntryTimeout = time.Second
	srv.redriveTimeout = 10 * time.Second

	failedAt := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	entry := func(id, subject string, age time.Duration) routing.DLQEntry {
		return routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: id, RouteID: "test-route",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: subject}),
			FailedAt: failedAt.Add(-age),
		})
	}
	seedDLQ(t, dlq,
		entry("e-slow", slowSubject, 2*time.Minute),
		entry("e-fast-1", "fast", time.Minute),
		entry("e-fast-2", "fast", 0),
	)

	rec := dlqDo(mux, redriveReq(`{"ids":["e-slow","e-fast-1","e-fast-2"]}`))
	require.Equal(t, http.StatusMultiStatus, rec.Code, rec.Body.String())

	body := decodeRedriveResponse(t, rec)
	assert.Equal(t, 2, body.Redriven, rec.Body.String())
	require.Len(t, body.Errors, 1, rec.Body.String())
	assert.Equal(t, "e-slow", body.Errors[0].ID)
	assert.True(t, strings.HasPrefix(body.Errors[0].Error, "inject failed:"),
		"slow entry error = %q", body.Errors[0].Error)

	ctx := context.Background()
	_, err := dlq.Get(ctx, "e-slow")
	require.NoError(t, err, "a failed inject must leave the entry in the DLQ")
	for _, id := range []string{"e-fast-1", "e-fast-2"} {
		_, err := dlq.Get(ctx, id)
		assert.True(t, errors.Is(err, shared.ErrNotFound), "%s must be removed after its redrive, got %v", id, err)
	}
}

// A lookup that outlives the entry bound is reported as a deadline, not as a
// missing entry, even while the batch itself still has budget left.
func TestRedriveLookupPastTheEntryBoundIsNotReportedMissing(t *testing.T) {
	store := &mockDLQStore{
		getFunc: func(ctx context.Context, _ string) (routing.DLQEntry, error) {
			<-ctx.Done()
			return routing.DLQEntry{}, ctx.Err()
		},
	}
	srv, mux := dlqServer(store)
	srv.redriveEntryTimeout = 50 * time.Millisecond
	srv.redriveTimeout = 5 * time.Second

	rec := dlqDo(mux, redriveReq(`{"ids":["e-stuck"]}`))
	require.Equal(t, http.StatusMultiStatus, rec.Code, rec.Body.String())

	body := decodeRedriveResponse(t, rec)
	require.Len(t, body.Errors, 1, rec.Body.String())
	assert.Equal(t, "e-stuck", body.Errors[0].ID)
	assert.Equal(t, "redrive deadline exceeded before entry lookup", body.Errors[0].Error)
}
