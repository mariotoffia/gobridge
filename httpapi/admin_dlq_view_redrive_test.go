package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
)

// autoRedriveFacts are the facts a removed-subscription dead-letter carries.
var autoRedriveFacts = map[string]string{
	routing.ExtraInfoSessionID:       "plant-a",
	routing.ExtraInfoSubscription:    "sensors/+/temp",
	routing.ExtraInfoManagedIdentity: "tcp://broker:1883|plant-a",
}

// seedRedriveFieldEntries writes one auto-redrive entry with facts and one
// manual entry without any.
func seedRedriveFieldEntries(t *testing.T) *http.ServeMux {
	t.Helper()
	mux, dlq, _ := redriveSetup(t)
	failedAt := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	seedDLQ(t, dlq,
		routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: "e-auto", RouteID: "test-route",
			Envelope:    *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "sensors/1/temp"}),
			FailedAt:    failedAt,
			RedriveMode: routing.RedriveAuto,
			ExtraInfo:   autoRedriveFacts,
		}),
		routing.NewDLQEntry(routing.DLQEntrySpec{
			ID: "e-manual", RouteID: "test-route",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s"}),
			FailedAt: failedAt.Add(time.Second),
		}),
	)
	return mux
}

// assertRedriveFields checks the raw JSON of one entry view: the auto entry
// names its mode and facts, the manual entry has an empty mode and an empty
// object (never null) for its facts.
func assertRedriveFields(t *testing.T, raw map[string]json.RawMessage) {
	t.Helper()
	var id string
	require.NoError(t, json.Unmarshal(raw["id"], &id))
	switch id {
	case "e-auto":
		assert.JSONEq(t, `"auto"`, string(raw["redrive_mode"]))
		var facts map[string]string
		require.NoError(t, json.Unmarshal(raw["extra_info"], &facts))
		assert.Equal(t, autoRedriveFacts, facts)
	case "e-manual":
		assert.JSONEq(t, `""`, string(raw["redrive_mode"]))
		assert.Equal(t, "{}", string(raw["extra_info"]))
	default:
		t.Fatalf("unexpected entry id %q", id)
	}
}

func TestDLQListShowsRedriveFields(t *testing.T) {
	mux := seedRedriveFieldEntries(t)

	rec := dlqDo(mux, dlqReq(http.MethodGet, "/api/v1/admin/dlq/messages", ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Messages, 2)
	for _, m := range body.Messages {
		assertRedriveFields(t, m)
	}
}

func TestDLQGetShowsRedriveFields(t *testing.T) {
	mux := seedRedriveFieldEntries(t)

	for _, id := range []string{"e-auto", "e-manual"} {
		rec := dlqDo(mux, dlqReq(http.MethodGet, "/api/v1/admin/dlq/messages/"+id, ""))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
		assertRedriveFields(t, raw)
	}
}
