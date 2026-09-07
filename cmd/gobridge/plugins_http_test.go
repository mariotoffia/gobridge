//go:build gobridge_http || gobridge_all

package main

import (
	"context"
	"testing"
	"time"

	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { expectedFamilies = append(expectedFamilies, "http") }

// TestHTTPFamily_RegistersDecoders verifies the HTTP kind reaches the composition root.
func TestHTTPFamily_RegistersDecoders(t *testing.T) {
	reg := ports.NewRegistry()
	require.NoError(t, registerAllDecoders(reg))
	assert.Contains(t, reg.Kinds(), "http")
	assert.Contains(t, compiledFamilies, "http")
}

// TestHTTPFamily_WiresFactoryMetrics exercises the real sender without network clients.
func TestHTTPFamily_WiresFactoryMetrics(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "without metrics"
		if enabled {
			name = "with metrics"
		}
		t.Run(name, func(t *testing.T) {
			recorder := &ports.RecordingExporter{}
			var metrics ports.MetricsExporter
			if enabled {
				metrics = recorder
			}
			cfg := &ports.BridgeConfig{
				Bridge:    ports.BridgeSettings{ID: "http-family"},
				Receivers: []ports.ReceiverDef{{ID: "in", Transport: "http"}},
				Senders: []ports.SenderDef{{
					ID: "out", Transport: "http",
					Config: &httptransport.Config{AtMostOnceAcceptLoss: true},
				}},
				Bindings: []ports.BindingDef{{ID: "target", SenderID: "out"}},
				Routes: []ports.RouteDef{{
					ID: "route", ReceiverID: "in", Bindings: []string{"target"},
					Policy: ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop", AllowRetryDrop: true},
				}},
			}
			sup := bridge.NewSupervisor(bridge.WithSupervisorLogger(discardLogger()))
			require.NoError(t, wireAllFactories(t.Context(), sup, discardLogger(), metrics))
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			var runErr error
			t.Cleanup(func() {
				cancel()
				wait.RequireClosed(t, done, 2*time.Second)
				assert.NoError(t, runErr)
			})
			go func() {
				runErr = sup.Run(ctx, cfg, nil)
				close(done)
			}()
			wait.Until(t, 2*time.Second, "Supervisor publishes the HTTP runtime", func() bool {
				select {
				case <-done:
					return true
				default:
					return sup.Runtime() != nil
				}
			})
			rt := sup.Runtime()
			require.NotNil(t, rt)
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID: "message", Subject: "orders", Payload: []byte(`{"order":"123"}`),
			})
			require.NoError(t, rt.Inject(t.Context(), "route", env))
			if enabled {
				assert.Len(t, recorder.FindEntries(httptransport.MetricSSENoSubscribers), 1)
			} else {
				assert.Empty(t, recorder.Entries())
			}
		})
	}
}
