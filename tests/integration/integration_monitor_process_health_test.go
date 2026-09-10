package integration_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// processHealthServer starts a real httpapi.Server on random ports over a real
// runtime, with a caller-controlled terminal signal standing in for the
// composition root's supervisor. It returns the monitor base URL and the knob
// that flips the process into its wedged state.
func processHealthServer(t *testing.T, rt *goruntime.Runtime) (string, *atomic.Bool) {
	t.Helper()

	var terminal atomic.Bool
	srv := httpapi.New(rt, httpapi.Config{
		AdminAddr:        ":0",
		MonitorAddr:      ":0",
		AdminAPIKey:      shared.NewSecret(testAdminAPIKey),
		MonitorAPIKey:    shared.NewSecret(testMonitorAPIKey),
		RuntimeProvider:  func() ports.Runtime { return rt },
		TerminalProvider: terminal.Load,
	}, httpapi.WithServerLogger(nil))
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	return srv.MonitorURL(), &terminal
}

// TestMonitorProbes_EmptyRuntimeIsNotReadyForTraffic proves the wire contract a
// load balancer and an orchestrator actually read when a bridge boots without a
// usable configuration: it is alive, so it is not restarted, but it is NOT
// ready, and deep health names the reason as empty rather than leaving an
// operator to infer it from a zero-length route list.
func TestMonitorProbes_EmptyRuntimeIsNotReadyForTraffic(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("process-health-empty"))
	if err := rt.Start(t.Context()); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	base, _ := processHealthServer(t, rt)

	live, _ := apiGet(t, base+"/api/v1/monitor/live", "")
	if live.StatusCode != http.StatusOK {
		t.Fatalf("an empty bridge is alive and must not be restarted; /live returned %d", live.StatusCode)
	}

	ready, _ := apiGet(t, base+"/api/v1/monitor/ready", "")
	if ready.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an empty bridge carries no traffic and must shed it; /ready returned %d", ready.StatusCode)
	}

	deep, body := apiGet(t, base+"/api/v1/monitor/deephealth", testMonitorAPIKey)
	if deep.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/deephealth must report not-ready for an empty bridge, got %d", deep.StatusCode)
	}
	if body["empty"] != true {
		t.Fatalf("/deephealth must name the empty state, got empty=%v", body["empty"])
	}
	if body["ready_for_traffic"] != false {
		t.Fatalf("an empty bridge must not claim ready_for_traffic, got %v", body["ready_for_traffic"])
	}
	if body["level"] != ports.LevelRunning.String() {
		t.Fatalf("an empty bridge must report level %q, got %v", ports.LevelRunning, body["level"])
	}
}

// TestMonitorLive_FailsClosedOnWedgedProcess proves the liveness probe follows
// the PROCESS, not just the runtime object it can see. A composition root whose
// swap and recovery both failed has no active runtime at all — the same shape a
// healthy swap window produces — so without the process-level terminal signal
// the probe answers 200 forever and the orchestrator never restarts a bridge
// that routes nothing.
func TestMonitorLive_FailsClosedOnWedgedProcess(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("process-health-wedged"))
	if err := rt.Start(t.Context()); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	base, terminal := processHealthServer(t, rt)

	before, _ := apiGet(t, base+"/api/v1/monitor/live", "")
	if before.StatusCode != http.StatusOK {
		t.Fatalf("a healthy process must answer /live with 200, got %d", before.StatusCode)
	}

	terminal.Store(true)

	after, body := apiGet(t, base+"/api/v1/monitor/live", "")
	if after.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a wedged process must fail /live so the orchestrator restarts it, got %d", after.StatusCode)
	}
	if body["status"] != "terminal" {
		t.Fatalf("/live must report the terminal status, got %v", body["status"])
	}
}
