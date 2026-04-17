package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/testutil/wait"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stratCfg(id string) *config.BridgeConfig {
	return &config.BridgeConfig{Bridge: config.BridgeSettings{ID: id}}
}

func mustRecv(t *testing.T, ch <-chan *config.BridgeConfig, timeout time.Duration) *config.BridgeConfig {
	t.Helper()
	return wait.RequireReceive(t, ch, timeout)
}

func mustClose(t *testing.T, ch <-chan *config.BridgeConfig, timeout time.Duration) {
	t.Helper()
	select {
	case _, ok := <-ch:
		require.False(t, ok, "expected channel close, got a value")
	case <-time.After(timeout):
		t.Fatal("timeout waiting for channel close")
	}
}

func assertNoEmission(t *testing.T, ch <-chan *config.BridgeConfig, window time.Duration) {
	t.Helper()
	wait.Silent(t, ch, window)
}

// --- Direct Strategy ---

// TestDirectStrategy_PassThrough validates that every input is emitted immediately with no delay.
func TestDirectStrategy_PassThrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 1)
	out := NewDirectStrategy().Filter(ctx, in)

	cfg := stratCfg("pass-through")
	in <- cfg

	got := mustRecv(t, out, time.Second)
	assert.Equal(t, "pass-through", got.Bridge.ID)
}

// TestDirectStrategy_MultipleChanges validates that N changes produce N emissions in order.
func TestDirectStrategy_MultipleChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 10)
	out := NewDirectStrategy().Filter(ctx, in)

	ids := []string{"a", "b", "c", "d", "e"}
	for _, id := range ids {
		in <- stratCfg(id)
	}

	for _, want := range ids {
		got := mustRecv(t, out, time.Second)
		assert.Equal(t, want, got.Bridge.ID)
	}
}

// TestDirectStrategy_ContextCancel validates that the output channel closes cleanly on context cancellation.
func TestDirectStrategy_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan *config.BridgeConfig)
	out := NewDirectStrategy().Filter(ctx, in)

	cancel()
	mustClose(t, out, time.Second)
}

// TestDirectStrategy_InputChannelClosed validates that the output channel closes when the input closes.
func TestDirectStrategy_InputChannelClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig)
	out := NewDirectStrategy().Filter(ctx, in)

	close(in)
	mustClose(t, out, time.Second)
}

// --- Debounced Strategy ---

// TestDebouncedStrategy_QuietWindow validates that a single config emits after the quiet period of silence.
func TestDebouncedStrategy_QuietWindow(t *testing.T) {
	const quiet = 80 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 1)
	out := NewDebouncedStrategy(quiet).Filter(ctx, in)

	in <- stratCfg("quiet-1")

	assertNoEmission(t, out, quiet/2)

	got := mustRecv(t, out, quiet+200*time.Millisecond)
	assert.Equal(t, "quiet-1", got.Bridge.ID)
}

// TestDebouncedStrategy_EmitsLatest validates that 5 rapid configs result in only the last one emitted.
func TestDebouncedStrategy_EmitsLatest(t *testing.T) {
	const quiet = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 5)
	out := NewDebouncedStrategy(quiet).Filter(ctx, in)

	for i := 1; i <= 5; i++ {
		in <- stratCfg("cfg-" + string(rune('0'+i)))
	}

	got := mustRecv(t, out, quiet+200*time.Millisecond)
	assert.Equal(t, "cfg-5", got.Bridge.ID)

	assertNoEmission(t, out, quiet+50*time.Millisecond)
}

// TestDebouncedStrategy_ResetOnNewChange validates that the timer resets on each new config, preventing premature emit.
func TestDebouncedStrategy_ResetOnNewChange(t *testing.T) {
	const quiet = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 1)
	out := NewDebouncedStrategy(quiet).Filter(ctx, in)

	in <- stratCfg("first")
	time.Sleep(quiet / 2)

	assertNoEmission(t, out, 0)

	in <- stratCfg("second")
	time.Sleep(quiet / 2)

	assertNoEmission(t, out, 0)

	got := mustRecv(t, out, quiet+200*time.Millisecond)
	assert.Equal(t, "second", got.Bridge.ID)
}

// TestDebouncedStrategy_ExactlyOneEmitPerBurst validates that a burst of 10 changes produces exactly 1 emission.
func TestDebouncedStrategy_ExactlyOneEmitPerBurst(t *testing.T) {
	const quiet = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 10)
	out := NewDebouncedStrategy(quiet).Filter(ctx, in)

	for i := 0; i < 10; i++ {
		in <- stratCfg("burst")
	}

	got := mustRecv(t, out, quiet+200*time.Millisecond)
	assert.Equal(t, "burst", got.Bridge.ID)

	assertNoEmission(t, out, quiet+50*time.Millisecond)
}

// TestDebouncedStrategy_ContextCancel_PendingTimer validates that cancelling while a debounce timer is active produces no emission and closes cleanly.
func TestDebouncedStrategy_ContextCancel_PendingTimer(t *testing.T) {
	const quiet = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan *config.BridgeConfig, 1)
	out := NewDebouncedStrategy(quiet).Filter(ctx, in)

	in <- stratCfg("pending")

	time.Sleep(quiet / 4)
	cancel()

	// Drain: channel should close without emitting the pending config.
	for cfg := range out {
		t.Fatalf("unexpected emission after cancel: %+v", cfg)
	}
}

// TestDebouncedStrategy_InputChannelClosed_PendingTimer validates that closing the input while a timer is pending emits the last config then closes.
func TestDebouncedStrategy_InputChannelClosed_PendingTimer(t *testing.T) {
	const quiet = 150 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 1)
	out := NewDebouncedStrategy(quiet).Filter(ctx, in)

	in <- stratCfg("last")
	time.Sleep(quiet / 4)
	close(in)

	got := mustRecv(t, out, time.Second)
	assert.Equal(t, "last", got.Bridge.ID)

	mustClose(t, out, time.Second)
}

// TestDebouncedStrategy_ZeroQuietPeriod validates that a zero quiet period degrades to pass-through behavior.
func TestDebouncedStrategy_ZeroQuietPeriod(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 3)
	out := NewDebouncedStrategy(0).Filter(ctx, in)

	ids := []string{"x", "y", "z"}
	for _, id := range ids {
		in <- stratCfg(id)
	}

	for _, want := range ids {
		got := mustRecv(t, out, time.Second)
		assert.Equal(t, want, got.Bridge.ID)
	}
}

// --- Windowed Strategy ---

// TestWindowedStrategy_QuietWindow validates that sparse changes behave like debounced (emit after quiet period).
func TestWindowedStrategy_QuietWindow(t *testing.T) {
	const (
		quiet    = 80 * time.Millisecond
		maxDelay = 500 * time.Millisecond
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 1)
	out := NewWindowedStrategy(quiet, maxDelay).Filter(ctx, in)

	in <- stratCfg("sparse")

	assertNoEmission(t, out, quiet/2)

	got := mustRecv(t, out, quiet+200*time.Millisecond)
	assert.Equal(t, "sparse", got.Bridge.ID)
}

// TestWindowedStrategy_MaxDelay validates that a continuous stream forces emit at maxDelay even without a quiet window.
func TestWindowedStrategy_MaxDelay(t *testing.T) {
	const (
		quiet    = 80 * time.Millisecond
		maxDelay = 150 * time.Millisecond
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig)
	out := NewWindowedStrategy(quiet, maxDelay).Filter(ctx, in)

	// Feed configs faster than the quiet period so the quiet timer never fires.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(quiet / 3)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				i++
				select {
				case in <- stratCfg("stream"):
				case <-ctx.Done():
					return
				}
				if i > 20 {
					return
				}
			}
		}
	}()

	// The maxDelay timer should force an emission.
	got := mustRecv(t, out, maxDelay+200*time.Millisecond)
	assert.Equal(t, "stream", got.Bridge.ID)

	cancel()
	<-done
}

// TestWindowedStrategy_MaxDelayResetAfterEmit validates that after a forced emit the max-delay timer resets for the next batch.
func TestWindowedStrategy_MaxDelayResetAfterEmit(t *testing.T) {
	const (
		quiet    = 80 * time.Millisecond
		maxDelay = 120 * time.Millisecond
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig)
	out := NewWindowedStrategy(quiet, maxDelay).Filter(ctx, in)

	// Continuously feed to force maxDelay emit.
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(quiet / 3)
		defer ticker.Stop()
		batch := 1
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				select {
				case in <- stratCfg("batch-" + itoa(batch)):
				case <-stop:
					return
				}
			}
		}
	}()

	// First forced emission.
	first := mustRecv(t, out, maxDelay+200*time.Millisecond)
	require.NotNil(t, first)

	// Second forced emission should arrive roughly maxDelay later.
	second := mustRecv(t, out, maxDelay+200*time.Millisecond)
	require.NotNil(t, second)

	close(stop)
}

// TestWindowedStrategy_QuietBeforeMaxDelay validates that the quiet window fires before max delay when changes stop early.
func TestWindowedStrategy_QuietBeforeMaxDelay(t *testing.T) {
	const (
		quiet    = 60 * time.Millisecond
		maxDelay = 500 * time.Millisecond
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 1)
	out := NewWindowedStrategy(quiet, maxDelay).Filter(ctx, in)

	start := time.Now()
	in <- stratCfg("early")

	got := mustRecv(t, out, maxDelay)
	elapsed := time.Since(start)

	assert.Equal(t, "early", got.Bridge.ID)
	assert.Less(t, elapsed, maxDelay, "should have emitted via quiet timer, not max delay")
}

// TestWindowedStrategy_ContextCancel validates that cancelling during a window produces no emission and closes cleanly.
func TestWindowedStrategy_ContextCancel(t *testing.T) {
	const (
		quiet    = 200 * time.Millisecond
		maxDelay = 500 * time.Millisecond
	)

	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan *config.BridgeConfig, 1)
	out := NewWindowedStrategy(quiet, maxDelay).Filter(ctx, in)

	in <- stratCfg("pending")
	time.Sleep(quiet / 4)
	cancel()

	for cfg := range out {
		t.Fatalf("unexpected emission after cancel: %+v", cfg)
	}
}

// TestWindowedStrategy_MaxDelayLessThanQuiet validates that when maxDelay < quietPeriod, the maxDelay always wins.
func TestWindowedStrategy_MaxDelayLessThanQuiet(t *testing.T) {
	const (
		quiet    = 200 * time.Millisecond
		maxDelay = 80 * time.Millisecond
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 1)
	out := NewWindowedStrategy(quiet, maxDelay).Filter(ctx, in)

	start := time.Now()
	in <- stratCfg("max-wins")

	got := mustRecv(t, out, quiet+200*time.Millisecond)
	elapsed := time.Since(start)

	assert.Equal(t, "max-wins", got.Bridge.ID)
	// Must have fired around maxDelay, well before quietPeriod.
	assert.Less(t, elapsed, quiet, "maxDelay should fire before quietPeriod")
}

// TestWindowedStrategy_EqualPeriods validates deterministic behavior when maxDelay == quietPeriod.
func TestWindowedStrategy_EqualPeriods(t *testing.T) {
	const period = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *config.BridgeConfig, 1)
	out := NewWindowedStrategy(period, period).Filter(ctx, in)

	in <- stratCfg("equal")

	got := mustRecv(t, out, period+200*time.Millisecond)
	assert.Equal(t, "equal", got.Bridge.ID)

	assertNoEmission(t, out, period+50*time.Millisecond)
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
