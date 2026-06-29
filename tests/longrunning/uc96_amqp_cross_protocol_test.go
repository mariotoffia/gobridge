//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// =========================================================================
// UC96: Cross-Protocol Bridge — RabbitMQ (AMQP 0-9-1) and Artemis (AMQP 1.0)
//
// Validates that both AMQP adapters work correctly in the same process and
// that message headers/payloads are preserved across both protocols.
//
// Phase 1: Send 500 messages directly to RabbitMQ -> collector verifies all
// Phase 2: Send 500 messages directly to Artemis  -> collector verifies all
// Phase 3: Verify header/payload preservation across both protocols
//
// This proves that both protocol adapters can coexist, share the same
// infrastructure (Docker containers), and produce/consume correctly.
//
// Assert: 500 messages received on each protocol, payload integrity.
// =========================================================================

func TestUC96_CrossProtocol_RabbitMQ_Artemis(t *testing.T) {
	const (
		msgCount    = 500
		testTimeout = 120 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// -----------------------------------------------------------------
	// Phase 1: RabbitMQ (AMQP 0-9-1)
	// -----------------------------------------------------------------
	exchange := rabbitmqlocal.UniqueExchange("uc96-ex")
	queue := rabbitmqlocal.UniqueQueue("uc96-q")
	routingKey := queue

	rabbitmqlocal.CreateExchange(t, exchange, "direct")
	rabbitmqlocal.CreateQueue(t, queue)
	rabbitmqlocal.BindQueue(t, queue, exchange, routingKey)

	rmqSess := setupRabbitMQSession(t, connectivity.SessionEphemeral)
	require.NoError(t, rmqSess.Reconcile(ctx, connectivity.SessionPlan{
		Publishers: []connectivity.PublisherPlan{
			{Topic: exchange},
		},
	}))
	rmqSender := newRabbitMQSender(t, rmqSess, exchange, routingKey)
	rmqCollector := newRabbitMQCollector(t, queue)

	t.Logf("UC96: Phase 1 — sending %d messages to RabbitMQ", msgCount)
	sendToRabbitMQ(t, rmqSender, msgCount, "uc96-rmq")

	lrWaitFor(t, 60*time.Second,
		fmt.Sprintf("RabbitMQ collector >= %d", msgCount),
		func() bool { return rmqCollector.count() >= msgCount })

	rmqReceived := rmqCollector.count()
	rmqUnique := countUniqueAMQP(rmqCollector)
	t.Logf("UC96: RabbitMQ received=%d, unique=%d", rmqReceived, rmqUnique)

	assert.GreaterOrEqual(t, rmqReceived, msgCount,
		"Phase 1: all %d messages must arrive via RabbitMQ", msgCount)

	// -----------------------------------------------------------------
	// Phase 2: Artemis (AMQP 1.0)
	// -----------------------------------------------------------------
	address := artemislocal.UniqueAddress("uc96-addr")

	artCollector := newArtemisCollector(t, address)
	artSess := setupArtemisSession(t, connectivity.SessionEphemeral)
	artSender := newArtemisSender(t, artSess, address)

	t.Logf("UC96: Phase 2 — sending %d messages to Artemis", msgCount)
	sendToArtemis(t, artSender, msgCount, "uc96-art")

	lrWaitFor(t, 60*time.Second,
		fmt.Sprintf("Artemis collector >= %d", msgCount),
		func() bool { return artCollector.count() >= msgCount })

	artReceived := artCollector.count()
	artUnique := countUniqueAMQP(artCollector)
	t.Logf("UC96: Artemis received=%d, unique=%d", artReceived, artUnique)

	assert.GreaterOrEqual(t, artReceived, msgCount,
		"Phase 2: all %d messages must arrive via Artemis", msgCount)

	// -----------------------------------------------------------------
	// Phase 3: Verify payload preservation across both protocols
	// -----------------------------------------------------------------
	rmqMsgs := rmqCollector.getMessages()
	artMsgs := artCollector.getMessages()

	rmqPayloads := make(map[string]bool, len(rmqMsgs))
	for _, m := range rmqMsgs {
		rmqPayloads[string(m.Payload())] = true
	}
	artPayloads := make(map[string]bool, len(artMsgs))
	for _, m := range artMsgs {
		artPayloads[string(m.Payload())] = true
	}

	// Both protocols should have the same set of sequential payloads.
	for i := 0; i < msgCount; i++ {
		expected := fmt.Sprintf(`{"seq":%d}`, i)
		assert.True(t, rmqPayloads[expected],
			"RabbitMQ missing payload for seq %d", i)
		assert.True(t, artPayloads[expected],
			"Artemis missing payload for seq %d", i)
	}

	t.Logf("UC96: Cross-protocol validation complete — RabbitMQ=%d, Artemis=%d",
		rmqReceived, artReceived)
}
