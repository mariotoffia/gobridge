package servicebus

import (
	"strconv"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// --- c6-empty-msgid: stable, entity-namespaced fallback ID -----------------

// TestASBReceivedToEnvelope_EmptyMessageIDStableFromSequenceNumber proves
// that a broker message with an empty MessageID maps to a STABLE envelope
// id derived from its (entity, sequence number) — which peek-lock
// redelivery preserves — so downstream idempotency/dedup collapses
// redeliveries of the same wire message onto one id while distinct
// messages (distinct sequence numbers) never collide.
//
// Mutation: the unfixed code assigns a fresh random generateEnvelopeID()
// for an empty MessageID, so the two same-sequence receives get different
// ids and the equality assertion fails.
func TestASBReceivedToEnvelope_EmptyMessageIDStableFromSequenceNumber(t *testing.T) {
	t.Parallel()

	clk := clocktest.NewAt(time.Now().UTC())
	const entity = "orders-q"
	const seq int64 = 987654321
	mk := func() *azservicebus.ReceivedMessage {
		s := seq
		return &azservicebus.ReceivedMessage{Body: []byte("b"), SequenceNumber: &s}
	}

	env1, err := receivedToEnvelope(mk(), clk, entity)
	require.NoError(t, err)
	env2, err := receivedToEnvelope(mk(), clk, entity) // same wire message, redelivered
	require.NoError(t, err)

	require.NotEmpty(t, env1.ID())
	require.Equal(t, "asb-seq:"+entity+":"+strconv.FormatInt(seq, 10), env1.ID())
	require.Equal(t, env1.ID(), env2.ID(),
		"an empty MessageID must map to a STABLE id across redeliveries (same entity+sequence number)")

	other := seq + 1
	env3, err := receivedToEnvelope(&azservicebus.ReceivedMessage{Body: []byte("b"), SequenceNumber: &other}, clk, entity)
	require.NoError(t, err)
	require.NotEqual(t, env1.ID(), env3.ID(),
		"distinct messages (distinct sequence numbers) must not collide")
}

// TestASBReceivedToEnvelope_EmptyMessageIDDistinctAcrossEntities is the
// cross-entity-DROP guard: the broker SequenceNumber is only unique WITHIN
// an entity, so two DIFFERENT entities can each assign sequence number 5.
// The fallback id must fold the entity in so a cross-entity dedup store
// never treats those two DISTINCT messages as one and DROPS a message.
//
// Mutation: drop the entity prefix from stableFallbackID (id becomes
// "asb-seq:5" for both) and the distinctness assertion fails.
func TestASBReceivedToEnvelope_EmptyMessageIDDistinctAcrossEntities(t *testing.T) {
	t.Parallel()

	clk := clocktest.NewAt(time.Now().UTC())
	const seq int64 = 5
	mk := func() *azservicebus.ReceivedMessage {
		s := seq
		return &azservicebus.ReceivedMessage{Body: []byte("b"), SequenceNumber: &s}
	}

	envQ1, err := receivedToEnvelope(mk(), clk, "q:queue-1")
	require.NoError(t, err)
	envQ2, err := receivedToEnvelope(mk(), clk, "q:queue-2")
	require.NoError(t, err)
	// A subscription scope is "s:<topic>:<subscription>" (entityScopeFor).
	envSub, err := receivedToEnvelope(mk(), clk, "s:topic:sub-a")
	require.NoError(t, err)

	require.NotEqual(t, envQ1.ID(), envQ2.ID(),
		"same sequence number on two different queues must produce DISTINCT ids (else a cross-entity dedup store drops one)")
	require.NotEqual(t, envQ1.ID(), envSub.ID(),
		"same sequence number on a queue and a subscription must produce DISTINCT ids")
}

// TestBuildRetryMessage_EmptyMessageIDAnchorsStableOriginalID proves a
// scheduled retry copy of an empty-MessageID source anchors its retry
// chain on the SAME (entity, sequence-number)-derived id, so the copy — a
// NEW broker message with its OWN sequence number — still maps back to the
// original's stable envelope id on redelivery.
//
// Mutation: without the empty-MessageID branch in buildRetryMessage the
// copy carries no OriginalMessageID and no MessageID, so on redelivery it
// mints an id from its own (different) sequence number and the final
// equality assertion fails.
func TestBuildRetryMessage_EmptyMessageIDAnchorsStableOriginalID(t *testing.T) {
	t.Parallel()

	clk := clocktest.NewAt(time.Now().UTC())
	const entity = "orders-q"
	const seq int64 = 42
	s := seq
	received := &azservicebus.ReceivedMessage{Body: []byte("b"), SequenceNumber: &s}

	out := buildRetryMessage(received, clk, entity)

	wantOriginal := "asb-seq:" + entity + ":" + strconv.FormatInt(seq, 10)
	orig, ok := out.ApplicationProperties[asbPropOriginalMessageID].(string)
	require.True(t, ok, "retry copy must carry a stable OriginalMessageID even with no source MessageID")
	require.Equal(t, wantOriginal, orig)
	require.NotNil(t, out.MessageID)
	require.Equal(t, wantOriginal+"-r"+strconv.Itoa(effectiveReceiveCount(received)), *out.MessageID)

	// The redelivered copy (different sequence number) must still resolve
	// to the ORIGINAL's stable envelope id via the preserved property.
	copySeq := seq + 1000
	redelivered := &azservicebus.ReceivedMessage{
		Body:                  out.Body,
		MessageID:             *out.MessageID,
		SequenceNumber:        &copySeq,
		ApplicationProperties: out.ApplicationProperties,
	}
	env, err := receivedToEnvelope(redelivered, clk, entity)
	require.NoError(t, err)
	require.Equal(t, wantOriginal, env.ID(),
		"a retry copy must trace back to the original's stable id, not its own sequence number")
}

// TestEntityScopeFor_Injective pins entityScopeFor as PROVABLY injective —
// no two distinct receive entities may share a fallback-ID scope, because a
// shared scope on a shared cross-entity dedup store would let a distinct
// message from one entity be dropped as a duplicate of the other (silent
// data loss).
//
// The load-bearing case is the collision the "/"-join scheme permitted:
// Azure allows "/" in queue AND topic names but forbids it in subscription
// names, and only bars a queue and topic sharing the SAME name — NOT a
// queue literally named "payment/processing" coexisting with topic
// "payment" + subscription "processing". Both minted the SAME "/"-joined
// scope. The ":"-prefixed scheme cannot collide: ":" is disallowed in every
// ASB entity name and the kinds differ in their first two chars.
//
// Mutation: revert entityScopeFor to the "/"-join scope (queue→name,
// subscription→topic+"/"+sub). Then entityScopeFor({QueueName:"payment/processing"})
// and entityScopeFor({TopicName:"payment", SubscriptionName:"processing"})
// both return "payment/processing" and the collision subtest FAILs.
func TestEntityScopeFor_Injective(t *testing.T) {
	t.Parallel()

	t.Run("queue path-name vs topic+subscription do NOT collide", func(t *testing.T) {
		t.Parallel()
		queueScope := entityScopeFor(ReceiverConfig{QueueName: "payment/processing"})
		subScope := entityScopeFor(ReceiverConfig{TopicName: "payment", SubscriptionName: "processing"})
		require.NotEqual(t, queueScope, subScope,
			"a queue named \"payment/processing\" must NOT share a scope with topic \"payment\"/subscription \"processing\" (else a shared dedup store drops a message)")
	})

	t.Run("distinct entities map to distinct scopes", func(t *testing.T) {
		t.Parallel()
		scopes := map[string]struct{}{}
		for _, cfg := range []ReceiverConfig{
			{QueueName: "orders"},
			{QueueName: "orders/eu"},
			{QueueName: "payment/processing"},
			{TopicName: "orders", SubscriptionName: "eu"},
			{TopicName: "payment", SubscriptionName: "processing"},
			{TopicName: "payment/processing", SubscriptionName: "eu"},
			{TopicName: "bare"},
		} {
			s := entityScopeFor(cfg)
			_, dup := scopes[s]
			require.Falsef(t, dup, "duplicate scope %q for %+v", s, cfg)
			scopes[s] = struct{}{}
		}
	})

	t.Run("scope shapes carry their kind prefix", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "q:orders", entityScopeFor(ReceiverConfig{QueueName: "orders"}))
		require.Equal(t, "s:t:sub", entityScopeFor(ReceiverConfig{TopicName: "t", SubscriptionName: "sub"}))
		require.Equal(t, "t:bare", entityScopeFor(ReceiverConfig{TopicName: "bare"}))
	})
}
