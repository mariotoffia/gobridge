package runtime_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// startClosedTransportSession starts rt with one session, s1, whose transport
// refuses to start because it is permanently closed: its manager ends with
// session.ErrSessionUnrecoverable over shared.ErrTransportClosedPermanently.
func startClosedTransportSession(t *testing.T, rt *goruntime.Runtime) {
	t.Helper()
	sess := NewFakeSession()
	sess.StartErr = fmt.Errorf("start after close: %w", shared.ErrTransportClosedPermanently)
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, sess, NewFakeSender()))
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
}

// With a session-unrecoverable handler that takes the rebuild, a session whose
// transport is permanently closed does not make the runtime terminal: the
// runtime keeps running, and the session stays not ready, and the instance not
// ready for traffic, until the handler's rebuild clears the fault.
func TestRuntime_RebuildableUnrecoverableSessionKeepsRuntimeRunningWithHandler(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	reports := make(chan string, 4)
	rt := goruntime.New(
		goruntime.WithInstanceID("unrecoverable-session-handler"),
		goruntime.WithClock(clk),
		goruntime.WithMetrics(rec),
		goruntime.WithSessionUnrecoverableHandler(func(sid string, _ error) bool {
			reports <- sid
			return true
		}),
	)
	startClosedTransportSession(t, rt)

	// The report waits out a jittered rebuild backoff on the injected clock.
	wait.Until(t, 5*time.Second, "the supervisor counts the rebuild the handler took", func() bool {
		clk.Advance(time.Second)
		return len(rec.FindEntries(shared.MetricSessionRebuilds)) == 1
	})
	assert.Equal(t, "s1", wait.RequireReceive(t, reports, 2*time.Second))

	assert.False(t, rt.Terminal(), "a session the handler rebuilds must not make the runtime terminal")
	assert.True(t, rt.IsRunning())
	assert.True(t, rt.SessionRebuildPending("s1"))

	dh := rt.DeepHealth(context.Background())
	require.Len(t, dh.Sessions, 1)
	assert.Equal(t, "s1", dh.Sessions[0].SessionID)
	assert.False(t, dh.Sessions[0].Ready)
	assert.Equal(t, ports.ServiceLevelNone, dh.Sessions[0].ServiceLevel)
	assert.False(t, dh.ReadyForTraffic)
}

// Without a handler the same failure makes the runtime terminal, so a process
// restart replaces the session.
func TestRuntime_RebuildableUnrecoverableSessionIsTerminalWithoutHandler(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("unrecoverable-session-no-handler"))
	startClosedTransportSession(t, rt)

	waitFor(t, 5*time.Second, "runtime terminal after an unrecoverable session", rt.Terminal)
	assert.False(t, rt.IsRunning())
	assert.ErrorIs(t, rt.ComponentErrors()["session:s1"], session.ErrSessionUnrecoverable)
}
