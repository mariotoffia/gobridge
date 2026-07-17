package paho

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

const (
	settlementRecoveryDrainLimit  = 5 * time.Second
	settlementRecoveryMinInterval = 30 * time.Second
)

func (s *Session) recoveryAttemptTimeout() time.Duration {
	s.mu.Lock()
	opts := s.opts
	mode := s.mode
	s.mu.Unlock()
	return (Config{Session: opts}).PostAcquireActivationTiming(mode).WorstCaseDuration
}

// contextWithClockTimeout applies a cancellable hard bound using the injected
// session clock, so recovery I/O observes deterministic cancellation in tests
// and no phase can outlive the adapter activation budget.
func (s *Session) contextWithClockTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if timeout <= 0 {
		cancel()
		return ctx, func() {}
	}
	timer := s.clock().NewTimer(timeout)
	done := make(chan struct{})
	go func() {
		select {
		case <-timer.C():
			cancel()
		case <-done:
		}
	}()
	var cancelOnce sync.Once
	return ctx, func() {
		cancelOnce.Do(func() {
			timer.Stop()
			close(done)
			cancel()
		})
	}
}

func (s *Session) beginRecoveryDrain(generation uint64) (<-chan struct{}, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryGeneration != generation {
		return nil, false, false
	}
	if s.recoveryDrainGeneration != generation {
		s.recoveryDrainState = recoveryDrainNotStarted
		s.recoveryDrainGeneration = generation
		s.recoveryDrainDone = nil
	}
	switch s.recoveryDrainState {
	case recoveryDrainNotStarted:
		done := make(chan struct{})
		s.recoveryDrainState = recoveryDrainInProgress
		s.recoveryDrainDone = done
		return done, true, false
	case recoveryDrainInProgress:
		return s.recoveryDrainDone, false, false
	case recoveryDrainFinished:
		return s.recoveryDrainDone, false, true
	default:
		return nil, false, false
	}
}

func (s *Session) finishRecoveryDrain(generation uint64, done <-chan struct{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryDrainGeneration != generation ||
		s.recoveryDrainState != recoveryDrainInProgress ||
		s.recoveryDrainDone != done {
		return false
	}
	s.recoveryDrainState = recoveryDrainFinished
	close(s.recoveryDrainDone)
	return true
}

func (s *Session) clearRecoveryDrainLocked(generation uint64) {
	if s.recoveryDrainGeneration != generation {
		return
	}
	s.recoveryDrainState = recoveryDrainNotStarted
	s.recoveryDrainGeneration = 0
	s.recoveryDrainDone = nil
}

func settlementRecoveryTerminalError(cause error) error {
	if cause == nil {
		cause = shared.ErrUnavailable.WithMessage("mqtt: settlement recovery failed")
	}
	return shared.ErrUnavailable.
		WithMessage("mqtt: settlement recovery failed; session is terminal").
		Wrap(errors.Join(cause, shared.ErrTransportClosedPermanently))
}

func (s *Session) terminateFailedRecovery(generation uint64, cause error, async bool) bool {
	terminal := settlementRecoveryTerminalError(cause)
	_, started := s.transitionTerminal(context.Background(), terminal, generation, async, false)
	return started
}

func (s *Session) failQueuedRecovery(generation uint64, attemptErr error) bool {
	return s.terminateFailedRecovery(generation, attemptErr, false)
}

func (s *Session) completeRecoveryAttempt(generation uint64, attemptErr error, success bool) bool {
	if !success {
		if attemptErr == nil {
			attemptErr = shared.ErrUnavailable.WithMessage("mqtt: settlement recovery did not complete")
		}
		return s.terminateFailedRecovery(generation, attemptErr, false)
	}

	s.mu.Lock()
	if !s.recoveryAttemptActive || s.recoveryGeneration != generation {
		s.mu.Unlock()
		return false
	}
	s.recoveryAttemptActive = false
	s.lastRecoveryCompleted = s.clock().Now()
	cancel := s.recoveryAttemptCancel
	s.recoveryAttemptCancel = nil
	s.recoveryPending = false
	s.recoveryNeedsSessionPresent = false
	s.recoverySessionPresentEpoch = 0
	s.recoveryTargetEpoch = 0
	s.clearRecoveryDrainLocked(generation)
	s.recoveryErr = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

func (s *Session) recordRecoveryRecycleStart(generation uint64) bool {
	s.mu.Lock()
	if !s.recoveryAttemptActive || s.recoveryGeneration != generation {
		s.mu.Unlock()
		return false
	}
	s.recoveryRecycleCount++
	s.mu.Unlock()
	s.metrics.Counter(MetricMQTTSessionRecoveryRecycle, 1,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})
	return true
}

func (s *Session) captureRecoveryTargetEpoch(generation uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.recoveryAttemptActive || s.recoveryGeneration != generation {
		return shared.ErrUnavailable.WithMessage("mqtt: settlement recovery attempt is no longer active")
	}
	targetEpoch := s.connEpoch
	s.recoveryTargetEpoch = targetEpoch
	if s.recoveryErr != nil {
		return s.recoveryErr
	}
	if targetEpoch == 0 || s.recoverySessionPresentEpoch != targetEpoch {
		return shared.ErrUnavailable.
			WithMessage("mqtt: Session Present evidence does not match recovery connection epoch").
			With("target_epoch", targetEpoch).
			With("session_present_epoch", s.recoverySessionPresentEpoch)
	}
	return nil
}

// requestRecovery records the Retry synchronously, then recycles the durable
// broker session asynchronously. Retry cannot perform the recycle inline: the
// recycle drain includes the RouteRunner settlement that is currently calling
// Retry, so waiting here would deadlock that drain.
func (s *Session) requestRecovery(ctx context.Context) error {
	if s.mode == connectivity.SessionEphemeral {
		return shared.ErrNotSupported
	}
	s.mu.Lock()
	if s.terminalErr != nil {
		terminal := s.terminalErr
		s.mu.Unlock()
		return terminal
	}
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("mqtt: session recovery requested after session closure")
	}
	if s.recoveryPending {
		s.mu.Unlock()
		return nil
	}
	s.recoveryPending = true
	s.recoveryNeedsSessionPresent = true
	s.recoverySessionPresentEpoch = 0
	s.recoveryDrainState = recoveryDrainNotStarted
	s.recoveryDrainGeneration = 0
	s.recoveryDrainDone = nil
	s.recoveryGeneration++
	generation := s.recoveryGeneration
	s.recoveryErr = nil
	s.subscriptionsSatisfied = false
	var rateLimit <-chan time.Time
	if !s.lastRecoveryCompleted.IsZero() {
		elapsed := s.clock().Since(s.lastRecoveryCompleted)
		if elapsed < settlementRecoveryMinInterval {
			rateLimit = s.clock().After(settlementRecoveryMinInterval - elapsed)
		}
	}
	s.mu.Unlock()

	go s.runRecovery(context.WithoutCancel(ctx), rateLimit, generation)
	return nil
}

func (s *Session) runRecovery(ctx context.Context, rateLimit <-chan time.Time, generation uint64) {
	if rateLimit != nil {
		select {
		case <-rateLimit:
		case <-ctx.Done():
			return
		}
	}
	attemptTimeout := s.recoveryAttemptTimeout()
	attemptCtx, cancelAttempt := s.contextWithClockTimeout(ctx, attemptTimeout)
	ctx = attemptCtx
	if err := s.acquireReload(ctx); err != nil {
		mapped := MapError(err).WithMessage("mqtt: settlement recovery waiting for session serialization")
		cancelAttempt()
		s.failQueuedRecovery(generation, mapped)
		return
	}
	defer s.releaseReload()
	if err := ctx.Err(); err != nil {
		cancelAttempt()
		s.failQueuedRecovery(generation,
			MapError(err).WithMessage("mqtt: settlement recovery cancelled after serialization"))
		return
	}

	// Request state becomes attempt state only after owning the single session
	// gate. An ordinary Reconcile that ran before this point saw only degraded,
	// coalescing request state and could not validate or complete this recovery.
	s.mu.Lock()
	if !s.recoveryPending || s.recoveryGeneration != generation {
		s.mu.Unlock()
		cancelAttempt()
		return
	}
	if s.closed || s.terminalErr != nil || s.recoveryErr != nil {
		queuedErr := s.recoveryErr
		if queuedErr == nil {
			queuedErr = s.terminalErr
		}
		if queuedErr == nil {
			queuedErr = shared.ErrUnavailable.WithMessage("mqtt: session closed before queued recovery began")
		}
		s.mu.Unlock()
		cancelAttempt()
		s.failQueuedRecovery(generation, queuedErr)
		return
	}
	s.recoveryAttemptActive = true
	s.recoveryDrainState = recoveryDrainNotStarted
	s.recoveryDrainGeneration = generation
	s.recoveryDrainDone = nil
	s.recoveryAttemptCancel = cancelAttempt
	s.recoveryNeedsSessionPresent = true
	s.recoverySessionPresentEpoch = 0
	s.recoveryTargetEpoch = 0
	s.mu.Unlock()

	// Exactly one owner drains accepted settlements for this generation. A
	// terminal caller joins drainDone rather than starting a second waiter.
	drainDone, drainOwner, drainFinished := s.beginRecoveryDrain(generation)
	if !drainOwner {
		if !drainFinished && drainDone != nil {
			select {
			case <-drainDone:
			case <-ctx.Done():
			}
		}
		return
	}
	drainCtx, cancelDrain := s.contextWithClockTimeout(ctx, settlementRecoveryDrainLimit)
	drainErr := s.quiesceForRecycle(drainCtx)
	cancelDrain()
	s.finishRecoveryDrain(generation, drainDone)
	if drainErr != nil {
		s.completeRecoveryAttempt(generation,
			shared.ErrUnavailable.WithMessage("mqtt: settlement recovery drain failed").Wrap(drainErr), false)
		return
	}

	if !s.recordRecoveryRecycleStart(generation) {
		return
	}
	if err := s.reloadLocked(ctx); err != nil {
		s.completeRecoveryAttempt(generation, err, false)
		return
	}
	if err := s.captureRecoveryTargetEpoch(generation); err != nil {
		s.completeRecoveryAttempt(generation, err, false)
		return
	}

	// Recovery owns the same gate as ordinary Reconcile, so converge the saved
	// plan through the private under-gate helper without publishing an event that
	// would need to reacquire serialization. The runtime manager may perform a
	// later idempotent Reconcile after this gate is released.
	s.mu.Lock()
	plan := connectivity.SessionPlan{}
	if s.plan != nil {
		plan = cloneSessionPlan(*s.plan)
	}
	s.mu.Unlock()
	_ = s.reconcileUnderGate(ctx, plan, generation)
}
