// Validates review-item-2 end-to-end: a durable receiver's Close forces a
// full teardown of the SHARED connection (the only way the pinned go-amqp
// can detach a durable link without an UNSUBSCRIBE), which transiently
// blips every sibling link on the same session. This test proves the
// blast radius is real AND that sibling recovery is BOUNDED — the sibling
// receiver reconnects, relatches, and resumes delivery with no permanent
// loss. It is the empirical backing for the dedicated-session contract
// documented in doc.go / README.md.
package amqp10

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
)

// TestIntegration_DurableClose_SiblingRecoveryBounded runs TWO links on ONE
// session — a durable receiver and a non-durable sibling receiver — and
// closes the durable one. It asserts:
//
//  1. Blast radius: closing the durable receiver tears down the shared
//     connection (a SessionDisconnected event fires), so the sibling link
//     is genuinely collateral-damaged — not an isolated close.
//  2. Bounded recovery: the session reconnects and the sibling receiver
//     relatches, so a message published AFTER the durable close is still
//     delivered. No permanent loss.
//
// Mutation killed: revert closeLink's durable branch to nil-only. Then the
// durable Close does NOT tear the connection down, the SessionDisconnected
// event never fires, and waitForEventType FAILs (the blast radius the
// contract warns about — and thus the reason for the teardown — is gone,
// but so is the durable-link detach the fix requires).
func TestIntegration_DurableClose_SiblingRecoveryBounded(t *testing.T) {
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()
	durableAddr := artemislocal.UniqueAddress("durable-sibling-durable")
	siblingAddr := artemislocal.UniqueAddress("durable-sibling-queue")

	const containerID = "gobridge-durable-sibling-test"

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Shared session A hosts BOTH links on ONE connection.
	sessA := NewSession(SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       shared.NewSecret(pass),
		ConnectTimeout: 15 * time.Second,
		ContainerID:    containerID,
	}, connectivity.SessionEphemeral, slog.Default())
	if err := sessA.Start(ctx); err != nil {
		t.Fatalf("sessA Start() error = %v", err)
	}
	defer func() { _ = sessA.Close(context.Background()) }()

	// Observe session-level events so we can prove the durable close really
	// tore down the shared connection (blast radius) and then recovered.
	events, unsub := sessA.Subscribe()
	defer unsub()

	// Durable sibling — a multicast durable subscription.
	durable, err := NewReceiver(ReceiverConfig{
		Address:          durableAddr,
		LinkCredit:       10,
		Session:          sessA,
		Routing:          RoutingMulticast,
		DurabilityMode:   2,
		SubscriptionName: "durable-sibling-sub",
	}, sessA)
	if err != nil {
		t.Fatalf("NewReceiver(durable): %v", err)
	}

	// Non-durable sibling — an anycast queue receiver that records the IDs
	// it delivers so we can assert it resumes after the teardown.
	got := make(chan string, 16)
	sibling, err := NewReceiver(ReceiverConfig{
		Address:    siblingAddr,
		LinkCredit: 10,
		Session:    sessA,
		Routing:    RoutingAnycast,
	}, sessA)
	if err != nil {
		t.Fatalf("NewReceiver(sibling): %v", err)
	}

	// Run the durable receiver just to attach it (unexpected deliveries fail).
	durCtx, durCancel := context.WithCancel(ctx)
	durDone := make(chan error, 1)
	go func() {
		durDone <- durable.Run(durCtx, func(c context.Context, del ports.Delivery) error {
			_ = del.Ack(c)
			return nil
		})
	}()

	// Run the sibling receiver: record + ack every delivery.
	sibCtx, sibCancel := context.WithCancel(ctx)
	defer sibCancel()
	sibDone := make(chan error, 1)
	go func() {
		sibDone <- sibling.Run(sibCtx, func(c context.Context, del ports.Delivery) error {
			id := del.Envelope().ID()
			if err := del.Ack(c); err != nil {
				return err
			}
			select {
			case got <- id:
			default:
			}
			return nil
		})
	}()

	// Wait for both links to attach on the shared connection.
	select {
	case <-durable.Started():
	case <-ctx.Done():
		t.Fatal("timeout waiting for durable link to attach")
	}
	select {
	case <-sibling.Started():
	case <-ctx.Done():
		t.Fatal("timeout waiting for sibling link to attach")
	}

	// Publisher lives on its OWN session so the sessA teardown cannot
	// affect the publish path — we measure the sibling RECEIVER's recovery.
	sessB := NewSession(SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       shared.NewSecret(pass),
		ConnectTimeout: 15 * time.Second,
	}, connectivity.SessionEphemeral, slog.Default())
	if err := sessB.Start(ctx); err != nil {
		t.Fatalf("sessB Start() error = %v", err)
	}
	defer func() { _ = sessB.Close(context.Background()) }()

	sender, err := NewSender(SenderConfig{
		Address: siblingAddr,
		Session: sessB,
		Routing: RoutingAnycast,
	}, sessB)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer func() { _ = sender.Close(context.Background()) }()

	publish := func(id string) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      id,
			Subject: "test.sibling",
			Payload: []byte(`{"k":"v"}`),
		})
		if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
			t.Errorf("Send(%s) error = %v", id, err)
		}
	}

	// Baseline: the sibling delivers a message BEFORE the durable close.
	publish("before-durable-close")
	waitForID(t, ctx, got, "before-durable-close", 30*time.Second)

	// Close the durable receiver. This forces a full teardown of sessA's
	// shared connection so the durable link is detached WITHOUT an
	// UNSUBSCRIBE — collaterally blipping the sibling on the same conn.
	durCancel()
	<-durDone
	if err := durable.Close(context.Background()); err != nil {
		t.Fatalf("durable Close: %v", err)
	}

	// Blast radius proof: the shared connection is torn down.
	waitForEventType(t, events, ports.SessionDisconnected, 30*time.Second)

	// Bounded recovery proof: after the teardown, keep publishing to the
	// sibling's queue until it relatches and resumes delivery. The anycast
	// queue may be briefly auto-deleted across the detach window, so we
	// retry the publish; a bounded recovery means the sibling MUST
	// eventually receive a post-teardown message (no permanent loss).
	recovered := false
	deadline := time.After(60 * time.Second)
	nextPublish := time.After(0)
	for !recovered {
		select {
		case <-ctx.Done():
			t.Fatal("context expired before sibling resumed delivery")
		case <-deadline:
			t.Fatal("sibling receiver never resumed delivery after durable close (permanent loss)")
		case id := <-got:
			if id == "after-durable-close" {
				recovered = true
			}
		case <-nextPublish:
			publish("after-durable-close")
			nextPublish = time.After(1 * time.Second)
		}
	}
}

// waitForID drains ch until it yields want or the timeout/ctx expires.
func waitForID(t *testing.T, ctx context.Context, ch <-chan string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("context expired waiting for delivery %q", want)
		case <-deadline:
			t.Fatalf("timeout waiting for delivery %q", want)
		case id := <-ch:
			if id == want {
				return
			}
		}
	}
}
