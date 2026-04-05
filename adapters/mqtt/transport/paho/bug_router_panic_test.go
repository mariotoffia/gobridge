package paho

import (
	"sync"
	"sync/atomic"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// GAP-11: MQTT Router Panic Recovery
//
// When a handler panics, the router must:
// 1. Recover without crashing the router
// 2. Emit MetricMQTTHandlerPanics counter
// 3. Still dispatch to other handlers for the same message
// 4. Correctly decrement WaitGroup so Wait() returns
// ═══════════════════════════════════════════════════════════════════════════

// TestRouterPanic_OtherHandlerStillRuns registers two handlers where the
// first panics. Verifies the second handler still receives the message,
// the panic metric is incremented, and Wait() returns without hanging.
func TestRouterPanic_OtherHandlerStillRuns(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec)

	var (
		handler2Called atomic.Bool
		wg            sync.WaitGroup
	)

	// Handler 1: panics
	r.Register("panic-handler", func(_ *pahov5.Publish) {
		panic("intentional test panic")
	})

	// Handler 2: succeeds
	wg.Add(1)
	r.Register("good-handler", func(_ *pahov5.Publish) {
		handler2Called.Store(true)
		wg.Done()
	})

	// Route a message
	r.Route(newTestPacketPublish("test/panic", []byte("hello")))

	// Wait for handler 2 to complete
	wg.Wait()

	// Wait for all router goroutines (including the panicked one's deferred recovery)
	r.Wait()

	assert.True(t, handler2Called.Load(),
		"second handler should still run despite first handler panicking")

	entries := rec.FindEntries(domain.MetricMQTTHandlerPanics)
	assert.Len(t, entries, 1,
		"MetricMQTTHandlerPanics should be incremented exactly once")

	received, dropped := r.Stats()
	assert.Equal(t, int64(1), received, "one message should have been received")
	assert.Equal(t, int64(0), dropped, "no messages should be dropped (handlers registered)")
}

// TestRouterPanic_WaitReturns verifies that Wait() returns even when all
// handlers panic (WaitGroup is correctly decremented in deferred recovery).
func TestRouterPanic_WaitReturns(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec)

	r.Register("panic1", func(_ *pahov5.Publish) { panic("boom1") })
	r.Register("panic2", func(_ *pahov5.Publish) { panic("boom2") })

	r.Route(newTestPacketPublish("test/all-panic", []byte("data")))

	// This must return without hanging. If WaitGroup is not decremented
	// properly after panic recovery, this will deadlock.
	r.Wait()

	entries := rec.FindEntries(domain.MetricMQTTHandlerPanics)
	assert.Len(t, entries, 2,
		"both panics should be counted")
}
