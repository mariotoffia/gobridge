package wait

import "time"

// TestingT is the subset of *testing.T used by wait helpers.
type TestingT interface {
	Helper()
	Fatalf(format string, args ...any)
	Deadline() (time.Time, bool)
}

// RequireReceive waits for a value on ch or fails the test if deadline elapses.
func RequireReceive[T any](t TestingT, ch <-chan T, deadline time.Duration) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(deadline):
		t.Fatalf("RequireReceive: timed out after %s waiting for value on channel", deadline)
		var zero T
		return zero
	}
}

// RequireClosed waits for ch to be closed or fails the test if deadline elapses.
func RequireClosed[T any](t TestingT, ch <-chan T, deadline time.Duration) {
	t.Helper()

	timer := time.NewTimer(deadline)
	defer timer.Stop()

	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timer.C:
			t.Fatalf("RequireClosed: timed out after %s waiting for channel to close", deadline)
			return
		}
	}
}

// Silent asserts that nothing is received on ch within window.
func Silent[T any](t TestingT, ch <-chan T, window time.Duration) {
	t.Helper()

	select {
	case v := <-ch:
		t.Fatalf("Silent: expected no value within %s but received %v", window, v)
	case <-time.After(window):
		return
	}
}

// StableFor polls sample() and asserts the value doesn't change for stableFor
// within an overall deadline. Returns the stable value.
func StableFor[T comparable](t TestingT, sample func() T, stableFor, deadline time.Duration) T {
	t.Helper()

	const pollInterval = 5 * time.Millisecond

	start := time.Now()
	current := sample()
	lastChange := start

	for {
		elapsed := time.Since(start)
		if elapsed >= deadline {
			t.Fatalf(
				"StableFor: value did not stay stable for %s within deadline %s (last change at %s)",
				stableFor, deadline, lastChange.Sub(start),
			)
			return current
		}

		if time.Since(lastChange) >= stableFor {
			return current
		}

		time.Sleep(pollInterval)

		v := sample()
		if v != current {
			current = v
			lastChange = time.Now()
		}
	}
}

// Until polls cond with exponential backoff (starting 1ms, capping at 50ms)
// until it returns true or deadline elapses.
func Until(t TestingT, deadline time.Duration, desc string, cond func() bool) {
	t.Helper()

	effectiveDeadline := time.Now().Add(deadline)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Before(effectiveDeadline) {
		effectiveDeadline = testDeadline
	}

	backoff := time.Millisecond
	const maxBackoff = 50 * time.Millisecond

	for {
		if cond() {
			return
		}

		if time.Now().After(effectiveDeadline) {
			t.Fatalf("Until: condition %q not met within %s", desc, deadline)
			return
		}

		remaining := time.Until(effectiveDeadline)
		sleep := backoff
		if sleep > remaining {
			sleep = remaining
		}
		time.Sleep(sleep)

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
