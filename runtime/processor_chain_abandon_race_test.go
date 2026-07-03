package runtime_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// TestRouteRunner_ChainTimeout_AbandonedProcessorDoesNotRaceSource is the
// finding-1 regression guard. On a processor-chain timeout the runner abandons
// the still-running processor goroutine (route/chain.go returns
// ErrProcessorTimeout without waiting). Before the fix that goroutine held the
// SAME *Envelope the runner then reads on the error path (receiveCount, DLQ
// serialization) — a late SetHeader from the abandoned goroutine raced the
// runner's concurrent header-map read, a fatal "concurrent map read and map
// write" crash.
//
// The fix runs the chain on an isolated clone (runner.go: chainEnv :=
// env.Clone()), confining any abandoned-goroutine mutation to the clone while
// the runner only ever reads the source envelope. This test drives many
// deliveries whose processor abandons mid-SetHeader (writing new keys forever)
// and asserts, under `-race`, that no data race occurs between the abandoned
// writers and the runner's source-envelope reads. If someone removes the clone
// (chainEnv == env), the race detector fails this test.
func TestRouteRunner_ChainTimeout_AbandonedProcessorDoesNotRaceSource(t *testing.T) {
	const deliveries = 16

	stop := make(chan struct{})
	var stopOnce sync.Once
	closeStop := func() { stopOnce.Do(func() { close(stop) }) }
	defer closeStop()

	var started sync.WaitGroup
	started.Add(deliveries)
	var startSignalled int32 // guarded by writersMu
	var writersMu sync.Mutex

	proc := &FakeProcessor{
		NameVal: "abandoner",
		ProcessFn: func(_ context.Context, env *messaging.Envelope, _ ports.ProcessorFunc) error {
			// Signal exactly once per goroutine that it is live.
			writersMu.Lock()
			startSignalled++
			writersMu.Unlock()
			started.Done()
			// Ignore cancellation entirely and keep mutating the envelope's
			// header map — this is the "abandoned" writer the chain timeout
			// leaves running. It writes ever-new keys so the map grows,
			// maximising the chance the race detector observes any accidental
			// sharing with the runner's concurrent reads.
			for i := 0; ; i++ {
				select {
				case <-stop:
					return context.Canceled
				default:
					env.SetHeader("late-"+strconv.Itoa(i), i)
				}
			}
		},
	}

	receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		// Small real timeout so the chain abandons the processor quickly.
		cfg.Policy.ProcessorTimeout = 30 * time.Millisecond
		cfg.Processors = []ports.Processor{proc}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	dels := make([]*FakeDelivery, deliveries)
	for i := range dels {
		// Each source envelope carries an SQS receive-count header so the
		// runner's error-path receiveCount() performs a real header-map read
		// concurrently with the abandoned writer.
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "msg-race-" + strconv.Itoa(i),
			Payload: []byte("data"),
			Headers: map[string]any{"sqs.ApproximateReceiveCount": 1},
		})
		dels[i] = NewFakeDelivery(env)
		if err := receiver.Emit(ctx, dels[i]); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}

	// Wait until every processor goroutine is live (writing) so the race
	// window with the runner's error-path reads is actually open.
	waitDone := make(chan struct{})
	go func() { started.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("processors did not all start")
	}

	// Wait for every delivery to reach a terminal outcome on the error path
	// (retry or settle). A processor timeout under the default policy retries.
	waitFor(t, 5*time.Second, "all deliveries settled on the timeout error path", func() bool {
		for _, d := range dels {
			if !d.IsRetried() && !d.IsAcked() {
				return false
			}
		}
		return true
	})

	// Let the abandoned writers unwind; the test passing under -race is the
	// assertion.
	closeStop()
}
