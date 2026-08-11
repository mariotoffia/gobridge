package servicebus

// partition_key_test.go —: PartitionKey must be settable on egress
// (asb.partition-key header) and PRESERVED in scheduled-retry copies so
// a retry on a partitioned entity without sessions keeps its ordering
// context instead of being re-hashed onto a different partition.

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// The critical fix: a scheduled retry copy must carry the original
// PartitionKey. Pre-fix buildRetryMessage dropped it, so a retry on a
// partitioned entity could land in a different partition.
func TestBuildRetryMessage_PreservesPartitionKey(t *testing.T) {
	t.Parallel()

	clk := clocktest.New()
	received := &azservicebus.ReceivedMessage{
		MessageID:     "m1",
		DeliveryCount: 1,
		PartitionKey:  strPtr("tenant-42"),
	}
	out := buildRetryMessage(received, clk, "q")

	require.NotNil(t, out.PartitionKey, "retry copy must carry a PartitionKey")
	require.Equal(t, "tenant-42", *out.PartitionKey)
}

// Egress: an asb.partition-key header must set Message.PartitionKey.
func TestEnvelopeToMessage_SetsPartitionKey(t *testing.T) {
	t.Parallel()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "e1",
		Payload: []byte("{}"),
		Headers: map[string]any{asbHeaderPartitionKey: "tenant-42"},
	})

	msg := envelopeToMessage(env, "", nil)

	require.NotNil(t, msg.PartitionKey, "asb.partition-key header must set PartitionKey")
	require.Equal(t, "tenant-42", *msg.PartitionKey)
}

// Ingress: a received PartitionKey must surface as an asb.partition-key
// header so a bridged message can carry it back onto egress.
func TestMessageToHeaders_SurfacesPartitionKey(t *testing.T) {
	t.Parallel()

	msg := &azservicebus.ReceivedMessage{
		MessageID:    "m1",
		PartitionKey: strPtr("tenant-42"),
	}

	h := messageToHeaders(msg)
	require.Equal(t, "tenant-42", h[asbHeaderPartitionKey])
}
