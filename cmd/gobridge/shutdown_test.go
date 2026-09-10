package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestShutdownSupervisor_ClosesObservability verifies ordered, once-only cleanup
// on normal shutdown, partial startup, supervisor failure and exhausted budgets.
func TestShutdownSupervisor_ClosesObservability(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		exited, expired, metrics, tracer, closeErr bool
	}{
		{name: "normal", metrics: true, tracer: true},
		{name: "supervisor exited", exited: true, metrics: true, tracer: true},
		{name: "tracer construction failed", exited: true, metrics: true},
		{name: "blank root", exited: true},
		{name: "close failure", metrics: true, tracer: true, closeErr: true},
		{name: "shutdown timed out", expired: true, metrics: true, tracer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			supDone := make(chan error, 1)
			if tc.expired {
				cancel()
			} else if !tc.exited {
				supDone <- nil
			}
			var log bytes.Buffer
			var closed []string
			var closeMetrics, closeTracer func(context.Context) error
			if tc.metrics {
				closeMetrics = func(got context.Context) error {
					assert.Same(t, ctx, got)
					assert.Empty(t, supDone, "supervisor result must be consumed before closing telemetry")
					closed = append(closed, "metrics")
					if tc.closeErr {
						return errors.New("export failed")
					}
					return nil
				}
			}
			if tc.tracer {
				closeTracer = func(got context.Context) error {
					assert.Same(t, ctx, got)
					closed = append(closed, "tracer")
					if tc.closeErr {
						return errors.New("trace export failed")
					}
					return nil
				}
			}

			shutdownSupervisor(ctx, tc.exited, supDone, captureLogger(&log), closeMetrics, closeTracer)

			var want []string
			if tc.metrics {
				want = append(want, "metrics")
			}
			if tc.tracer {
				want = append(want, "tracer")
			}
			assert.Equal(t, want, closed)
			if tc.closeErr {
				assert.Contains(t, log.String(), "export failed")
				assert.Contains(t, log.String(), "trace export failed")
			}
			if tc.expired {
				assert.Contains(t, log.String(), "supervisor shutdown timed out")
			} else {
				assert.NotContains(t, log.String(), "supervisor shutdown timed out")
			}
		})
	}
}
