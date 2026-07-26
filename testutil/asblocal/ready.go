package asblocal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// Protocol-truth readiness for the Service Bus emulator.
//
// The emulator's first AMQP roundtrip can lag well behind container start: it
// has to reach SQL, restore its entities and bring the broker up. None of the
// container-level gates observe that. WaitHealthy reports only State.Running,
// and the published AMQP port is bound by docker-proxy the moment the
// container is created, so a TCP dial succeeds regardless of whether the
// broker is listening — or alive.
//
// The gate below therefore does what dockerexec.WaitProbe was written for: a
// real send + receive on the pre-provisioned test queue. Only that proves the
// emulator accepts a link, persists a message and delivers it back.

const (
	// The emulator is the slowest fixture to come up (SQL restore + entity
	// provisioning), so the budget is generous; the probe returns as soon as
	// one roundtrip succeeds, so a healthy emulator costs only its real
	// startup time.
	emulatorReadyTimeout = 150 * time.Second

	readyProbeInterval  = 2 * time.Second
	readyAttemptTimeout = 10 * time.Second
)

// waitEmulatorReady blocks until the emulator completes a send/receive
// roundtrip on TestQueue, or timeout elapses.
func waitEmulatorReady(connStr string, timeout time.Duration) error {
	return dockerexec.WaitProbe("Service Bus emulator AMQP roundtrip", timeout,
		readyProbeInterval, func() error { return emulatorRoundtrip(connStr) })
}

// emulatorRoundtrip sends one message to TestQueue and receives it back,
// leaving the queue as it found it.
func emulatorRoundtrip(connStr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), readyAttemptTimeout)
	defer cancel()

	client, err := azservicebus.NewClientFromConnectionString(connStr, nil)
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	defer func() { _ = client.Close(ctx) }()

	sender, err := client.NewSender(TestQueue, nil)
	if err != nil {
		return fmt.Errorf("sender: %w", err)
	}
	defer func() { _ = sender.Close(ctx) }()

	body := []byte(fmt.Sprintf("gobridge-ready-%d", time.Now().UnixNano()))
	if err := sender.SendMessage(ctx, &azservicebus.Message{Body: body}, nil); err != nil {
		return fmt.Errorf("send: %w", err)
	}

	receiver, err := client.NewReceiverForQueue(TestQueue, nil)
	if err != nil {
		return fmt.Errorf("receiver: %w", err)
	}
	defer func() { _ = receiver.Close(ctx) }()

	// Drain what we just sent so the probe leaves no residue for the tests.
	// A retried attempt may also find messages from an earlier attempt, so
	// settle everything received rather than only our own message.
	msgs, err := receiver.ReceiveMessages(ctx, 10, nil)
	if err != nil {
		return fmt.Errorf("receive: %w", err)
	}
	if len(msgs) == 0 {
		return errors.New("no message delivered back")
	}
	for _, m := range msgs {
		if err := receiver.CompleteMessage(ctx, m, nil); err != nil {
			return fmt.Errorf("complete: %w", err)
		}
	}
	return nil
}
