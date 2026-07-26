package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// gatedSenderFactory lets a test hold a runtime build open at a precise point.
// The FIRST build (the initial runtime) passes straight through; every later one
// — i.e. a reconfiguration swap — signals `entered` and then blocks until the
// test closes `release`. That gives a deterministic "the drive goroutine is
// mid-swap right now" barrier with no sleeps and no timing assumptions.
type gatedSenderFactory struct {
	fakeTransportFactory
	entered chan struct{}
	release chan struct{}
	builds  chan struct{} // buffered; one token per NewSender call
}

func newGatedSenderFactory() *gatedSenderFactory {
	return &gatedSenderFactory{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		builds:  make(chan struct{}, 64),
	}
}

func (f *gatedSenderFactory) NewSender(_ context.Context, _ ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	select {
	case f.builds <- struct{}{}:
	default:
	}
	if len(f.builds) > 1 {
		select {
		case <-f.entered:
			// already signalled by an earlier gated build
		default:
			close(f.entered)
		}
		<-f.release
	}
	return &fakeSender{}, nil
}

// TestSupervisorRun_StopsTheRolloutDriveBeforeDrainingTheBridge pins the
// shutdown ORDER, which is not a detail: a committed rollout is applied on the
// DRIVE's goroutine, not on Run's. If Run drained the bridge first and only then
// stopped the drive, the drive could finish building and START a replacement
// runtime after the old one was already stopped — a fully-started runtime (live
// consumers, held lease, open store handles) that nothing references and nothing
// will ever Stop.
//
// The test holds a committed rollout's swap open at NewSender, cancels Run, and
// requires that Run does not return until the drive has unwound. A Run that
// returned while the swap was still in flight would be the bug.
func TestSupervisorRun_StopsTheRolloutDriveBeforeDrainingTheBridge(t *testing.T) {
	store := memoryrollout.NewStore()
	rc := testRolloutConfig(store, "node-a")
	rc.PollInterval = 5 * time.Millisecond
	rc.LeaseTTL = 20 * time.Millisecond

	tf := newGatedSenderFactory()
	onSwap, swaps := swapChan(8)
	s := newTestSupervisorTransport(tf, WithOnSwap(onSwap), WithClusterRollout(rc))

	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, soloCohortConfig(0), changes)
	// Cancel only. This test READS errCh itself, and errCh has capacity 1, so a
	// cleanup that also received from it would block forever on the second read.
	// Releasing the build gate is what lets Run finish, so it must also happen on
	// an early failure or Run never returns at all.
	t.Cleanup(cancel)
	t.Cleanup(func() {
		select {
		case <-tf.release:
		default:
			close(tf.release)
		}
	})

	candidate := soloCohortConfig(99)
	candidate.Bindings[0].Address = "addr/rolled"
	require.True(t, sendConfig(changes, candidate, time.Second))
	require.True(t, awaitSwap(t, swaps).Deferred, "the delta defers to the barrier")

	// Wait until the barrier has committed and the drive goroutine is INSIDE the
	// resulting swap, blocked in NewSender.
	// The drive goroutine must reach the committed swap.
	wait.RequireReceive(t, tf.entered, 10*time.Second)

	cancel()

	// Run must still be blocked: it cannot return while the drive is mid-swap.
	// A result arriving here means Run drained the bridge and returned while the
	// drive was still building — and starting — a replacement runtime.
	wait.Silent(t, errCh, 200*time.Millisecond)

	close(tf.release)
	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the in-flight swap was released")
	}
}

// TestStartRolloutDrive_StopIsIdempotent proves the stop function can be called
// twice without hanging or panicking. Run holds it as both an explicit ordered
// shutdown step and a deferred backstop, so double invocation is the normal
// path, not an edge case.
func TestStartRolloutDrive_StopIsIdempotent(t *testing.T) {
	store := memoryrollout.NewStore()
	rc := testRolloutConfig(store, "node-a")
	rc.PollInterval = 5 * time.Millisecond
	s := newTestSupervisor(WithClusterRollout(rc))
	s.cfg = soloCohortConfig(0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := s.startRolloutDrive(ctx)
	require.NotNil(t, stop)

	stop()
	stop() // must not hang or panic
}
