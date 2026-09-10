package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// Post-acquire activation: the bounded Start/Reconcile sequence a term runs
// while the renewal loop it shares is already renewing, and the fail-closed
// teardown when that sequence overruns its deadline or loses the lease under it.

// postAcquireActivationDeadline returns the hard deadline for initial
// Start/Reconcile. Production composition roots configure the transport's
// conservative whole-activation bound; direct-manager callers that leave it
// zero retain the legacy fallback to LeaseTTL minus the teardown margin.
func (m *Manager) postAcquireActivationDeadline() (time.Time, time.Duration, error) {
	now := m.clk.Now()
	if m.activationTimeout > 0 {
		return now.Add(m.activationTimeout), m.activationTimeout, nil
	}

	m.mu.Lock()
	leaseDeadline := m.leaseDeadline
	m.mu.Unlock()
	if leaseDeadline.IsZero() {
		return time.Time{}, 0, shared.ErrUnavailable.WithMessage("exclusive post-acquire activation has no local lease deadline")
	}
	margin := m.releaseTimeout()
	safeDeadline := leaseDeadline.Add(-margin)
	remaining := safeDeadline.Sub(now)
	if remaining <= 0 {
		return safeDeadline, 0, shared.NewBridgeError(shared.ErrCodeTimeout, shared.ErrorTransient,
			"exclusive post-acquire activation has no safe lease time remaining")
	}
	return safeDeadline, remaining, nil
}

// runPostAcquireActivation runs Start/Reconcile under the configured hard
// activation bound. Lease renewal is owned by the concurrently running, existing
// renewLoop; this helper only classifies callback completion and timeout.
func (m *Manager) runPostAcquireActivation(
	ctx context.Context,
	fn func(context.Context) error,
) (deadlineExceeded, completed bool, err error) {
	deadline, budget, budgetErr := m.postAcquireActivationDeadline()
	if budgetErr != nil {
		return true, true, budgetErr
	}

	activationCtx, cancel := context.WithTimeout(ctx, budget)
	err, completed = m.boundedCallResult(activationCtx, budget, "exclusive post-acquire activation", fn)
	activationCtxErr := activationCtx.Err()
	cancel()

	deadlineExceeded = !completed || errors.Is(activationCtxErr, context.DeadlineExceeded) || m.clk.Now().After(deadline)
	if !deadlineExceeded {
		return false, completed, err
	}
	cause := shared.NewBridgeError(shared.ErrCodeTimeout, shared.ErrorTransient,
		"exclusive post-acquire activation exceeded its configured hard deadline")
	if err != nil {
		cause = cause.Wrap(err)
	}
	return true, completed, cause
}

// leaseTermResult separates activation failure from the steady renewal-loop
// exit so callers preserve the existing phase-specific error handling.
type leaseTermResult struct {
	activationErr             error
	activationDeadlineHandled bool
	renewErr                  error
	terminalErr               error
}

// runRenewingActivation starts the existing renewal loop immediately after
// Acquire, gates session events until activation converges, and then leaves that
// same loop running for the rest of the ownership term. Exactly one renewer is
// active. Lease loss cancels activation and disconnects before this returns.
func (m *Manager) runRenewingActivation(
	ctx context.Context,
	token persistence.LeaseToken,
	fn func(context.Context) error,
) leaseTermResult {
	// Every lease term funnels through here, so this is where per-term
	// broker-path state is reset — before the renewal loop can read it.
	m.beginBrokerPathTerm()

	activationCtx, cancelActivation := context.WithCancel(ctx)
	defer cancelActivation()
	renewCtx, cancelRenew := context.WithCancel(ctx)
	defer cancelRenew()

	activationReady := make(chan struct{})
	renewStarted := make(chan struct{})
	renewDone := make(chan error, 1)
	go func() {
		renewDone <- m.renewLoop(renewCtx, activationReady, renewStarted, cancelActivation)
	}()
	<-renewStarted

	deadlineExceeded, completed, activationErr := m.runPostAcquireActivation(activationCtx, fn)
	if activationErr == nil {
		// Arm BEFORE events start flowing, so no disconnect can be handled
		// against an un-armed broker-health clock.
		m.markActivated(ctx)
		close(activationReady)
		renewErr := <-renewDone
		if lossResult := m.finishActivationLeaseLoss(ctx, renewErr, completed); lossResult != nil {
			if errors.Is(lossResult, errLeaseLostAfterRenewal) {
				return leaseTermResult{renewErr: lossResult}
			}
			return leaseTermResult{terminalErr: lossResult}
		}
		return leaseTermResult{renewErr: renewErr}
	}

	cancelRenew()
	renewErr := <-renewDone
	if lossResult := m.finishActivationLeaseLoss(ctx, renewErr, completed); lossResult != nil {
		if errors.Is(lossResult, errLeaseLostAfterRenewal) {
			return leaseTermResult{renewErr: lossResult}
		}
		return leaseTermResult{terminalErr: lossResult}
	}

	if deadlineExceeded {
		return leaseTermResult{
			activationErr:             m.failPostAcquireActivation(ctx, token, activationErr, completed),
			activationDeadlineHandled: true,
		}
	}
	return leaseTermResult{activationErr: activationErr}
}

// finishActivationLeaseLoss completes step-down only after activation has
// settled. A parked activation or failed disconnect is terminal and the lease is
// deliberately not released underneath work that may still mutate/send.
func (m *Manager) finishActivationLeaseLoss(ctx context.Context, renewErr error, activationCompleted bool) error {
	var loss *activationLeaseLoss
	if !errors.As(renewErr, &loss) {
		return nil
	}
	if !activationCompleted || !loss.closeCompleted {
		return fmt.Errorf("%w: lease lost during post-acquire activation before source work quiesced", ErrSessionUnrecoverable)
	}
	stepErr := m.finishStepDown(ctx, loss.token, true)
	if errors.Is(renewErr, errBrokerPathStepDown) {
		// finishStepDown returns the bare lease-loss sentinel, which would drop
		// the marker that stops this process re-seizing the lease it released.
		return fmt.Errorf("%w: %w", errBrokerPathStepDown, stepErr)
	}
	return stepErr
}

// failPostAcquireActivation removes local authorization, disconnects and
// quiesces the source, then releases only when both activation and teardown
// completed. A still-parked activation or Close retains the lease until natural
// expiry so no new owner can overlap work that may still mutate/send.
func (m *Manager) failPostAcquireActivation(
	ctx context.Context,
	token persistence.LeaseToken,
	cause error,
	activationCompleted bool,
) error {
	m.mu.Lock()
	m.hasLease = false
	m.mu.Unlock()
	terminal := fmt.Errorf("%w: post-acquire activation failed closed: %w", ErrSessionUnrecoverable, cause)
	_, closed := m.closeSourceBounded(ctx, m.releaseTimeout(), "post-acquire activation deadline")
	if activationCompleted && closed {
		m.releaseOwnedLeaseBestEffort(ctx, token, "post-acquire activation deadline")
	}
	return terminal
}
