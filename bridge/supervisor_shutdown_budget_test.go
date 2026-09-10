package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// blockingSessionFactory never returns from NewSession, standing in for a dial
// against a partitioned broker or a credential resolve that never completes.
type blockingSessionFactory struct {
	fakeTransportFactory
	entered chan struct{}
}

func (f *blockingSessionFactory) NewSession(ctx context.Context, _ ports.SessionSpec) (ports.Session, error) {
	select {
	case f.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestSupervisorRun_InitialBuildIsBounded: every reload build runs under the
// swap deadline, but the INITIAL build did not. A composition root without its
// own outer wait therefore blocked in Run forever on a hung construction call —
// no runtime, no health surface, no terminal signal, nothing for an orchestrator
// to act on.
func TestSupervisorRun_InitialBuildIsBounded(t *testing.T) {
	tf := &blockingSessionFactory{entered: make(chan struct{}, 1)}
	s := NewSupervisor(
		WithSupervisorBlueprintValidator(config.Validate),
		WithSwapDeadline(150*time.Millisecond),
	)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", tf)
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx, supervisorTestConfigWithSession("r1", "s1"), nil) }()

	wait.RequireReceive(t, tf.entered, 5*time.Second)

	select {
	case err := <-errCh:
		require.Error(t, err, "a hung initial build must fail, not start a bridge")
		assert.Contains(t, err.Error(), "initial build")
	case <-time.After(5 * time.Second):
		t.Fatal("initial build was not bounded: Run never returned while construction hung")
	}
}

// blockingReleaseLease is an election lease store whose Release never returns
// until its context ends. The coordinator resigns its lease on the way out of
// the drive goroutine under a DETACHED context bounded only by the lease TTL, so
// a store that will not release holds the drive goroutine — and any caller
// waiting on it — for the whole TTL.
type blockingReleaseLease struct {
	*electionLeaseStore
	entered chan struct{}
}

func (s *blockingReleaseLease) Release(ctx context.Context, _ string, _ persistence.LeaseToken) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestSupervisorRun_ShutdownDoesNotWaitForeverOnTheRolloutDrive: the process
// shutdown budget must cover the rollout drive too. The stop function waited on
// the drive goroutine with no bound at all, and that goroutine resigns the
// coordinator lease on its way out under a context bounded only by the LEASE TTL
// (45s in the HA profile, 6 minutes by default). A store that would not release
// therefore held SIGTERM ahead of the runtime drain and the HTTP shutdown, until
// the platform SIGKILLed the process mid-drain.
func TestSupervisorRun_ShutdownDoesNotWaitForeverOnTheRolloutDrive(t *testing.T) {
	lease := &blockingReleaseLease{
		electionLeaseStore: newElectionLeaseStore(),
		entered:            make(chan struct{}, 1),
	}
	rc := testRolloutConfig(memoryrollout.NewStore(), "node-a")
	rc.Lease = lease
	rc.PollInterval = 5 * time.Millisecond
	// Far longer than the shutdown budget below: the resign wait is what the
	// budget has to cut short.
	rc.LeaseTTL = 30 * time.Second

	s := newTestSupervisor(WithClusterRollout(rc))
	s.cfg = soloCohortConfig(0)

	driveCtx, driveCancel := context.WithCancel(context.Background())
	defer driveCancel()
	stop := s.startRolloutDrive(driveCtx)
	require.NotNil(t, stop)

	// Let the coordinator win the election so it has a lease to resign.
	require.Eventually(t, func() bool { return lease.Owner() != "" },
		5*time.Second, 5*time.Millisecond, "test setup: the coordinator must hold the lease")

	// The shutdown budget, not the lease TTL, decides how long this waits.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stopCancel()

	done := make(chan struct{})
	go func() { defer close(done); stop(stopCtx) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stopping the rollout drive ignored the shutdown budget and waited out the lease TTL")
	}
	wait.RequireReceive(t, lease.entered, 5*time.Second)
}
