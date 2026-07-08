package bootstrap

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// These tests cover audit chunk C18 finding 1: the bootstrap composition root
// never drained the HTTP transport's SSE senders on config reload or shutdown.
// adapters/http/transport.Factory.Close (invoked here via App.closeSupersededHTTP
// on swap and via the Stop drain block) unblocks the long-lived SSE handlers so
// (a) a fronting transport server.Shutdown does not hang the full budget on an
// open event stream, and (b) subscribers pinned to a superseded sender are
// released to reconnect to the freshly installed one instead of receiving only
// heartbeats forever. Both tests drive the real receiver -> route -> SSE-sender
// path over the App's transport server; neither uses time.Sleep for
// synchronization (they poll bounded deadlines or observe the live SSE stream).

// sseRouteConfig builds the minimal logical config the filesystem profile
// accepts that still mounts a drivable SSE sender: an HTTP receiver feeding a
// single route bound to an HTTP SSE sender. on_expired/on_permanent_failure are
// "drop" because this profile provisions no DLQ store (validateFilesystemProfile
// rejects the distributed features a DLQ would need). The SSE sender's logical
// identity is its spec ID ("sse") — the filesystem profile wires no route
// locator, so the runtime never calls SetRouteID — hence the binding address is
// the sender ID. A short heartbeat keeps the "stream is flowing" probes prompt.
func sseRouteConfig(bridgeID, logLevel string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             bridgeID,
			DeploymentMode: "standalone",
			LogLevel:       logLevel,
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx", Transport: "http", Config: httptransport.Config{Path: "/ingress"}},
		},
		Senders: []ports.SenderDef{
			{ID: "sse", Transport: "http", Config: httptransport.Config{
				Mode:              "sse",
				Path:              "/events",
				HeartbeatInterval: 100 * time.Millisecond,
			}},
		},
		Bindings: []ports.BindingDef{
			{ID: "to-sse", SenderID: "sse", Address: "sse"},
		},
		Routes: []ports.RouteDef{
			{
				ID:         "r1",
				ReceiverID: "rx",
				Bindings:   []string{"to-sse"},
				Policy:     ports.PolicyDef{OnExpired: "drop", OnPermanentFailure: "drop"},
			},
		},
	}
}

// startAppWithSSE starts an App on ephemeral ports and installs sseRouteConfig,
// returning the running App. The config is installed through applyLogicalConfig
// under a.mu — the same deterministic seam TestApp_AdminConfigEndpointReturns...
// uses — so no file watcher or sleep is involved. PollInterval is long so the
// watcher stays dormant; reloads are driven explicitly via reloadConfig.
func startAppWithSSE(t *testing.T, bridgeID string) *App {
	t.Helper()
	cfgPath := t.TempDir() + "/bridge.yaml"
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:          bridgeID,
		ConfigFilePath:    cfgPath,
		PollInterval:      "1h",
		AdminAddr:         ":0",
		MonitorAddr:       ":0",
		TransportHTTPAddr: ":0",
		AdminAPIKeyParam:  "/admin",
	}, WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}))

	require.NoError(t, app.Start(t.Context()))
	// Idempotent backstop: test 1 Stops explicitly; this no-ops afterward
	// (Stop returns immediately once started=false).
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	reloadConfig(t, app, sseRouteConfig(bridgeID, "info"))
	return app
}

// reloadConfig installs cfg through the same in-band apply path the file watcher
// and admin-commit hook use, serialized under a.mu. For the HTTP transport
// (no CapExclusiveIdentity) this is an overlap swap: a fresh factory registry is
// built and installed, and the previous one is superseded.
func reloadConfig(t *testing.T, app *App, cfg *ports.BridgeConfig) {
	t.Helper()
	app.mu.Lock()
	err := app.applyLogicalConfig(t.Context(), cfg)
	app.mu.Unlock()
	require.NoError(t, err)
}

// sseStream is a real HTTP SSE client reading the transport server's event
// stream on a background goroutine. Lines are pushed to a buffered channel; the
// channel closes when the stream ends (the server finished the response, e.g.
// after the sender was drained or the server shut down).
type sseStream struct {
	t     *testing.T
	resp  *http.Response
	lines chan string
	stop  chan struct{}
	once  sync.Once
}

// dialSSE connects an SSE client to url and asserts the stream is established
// (HTTP 200 — at which point the sender has already registered the client, see
// SSESender.ServeHTTP which flushes the header AFTER inserting into its client
// map). It starts the reader goroutine and returns the stream.
func dialSSE(t *testing.T, ctx context.Context, url string) *sseStream {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "SSE connect must establish the stream (200)")

	s := &sseStream{
		t:     t,
		resp:  resp,
		lines: make(chan string, 256),
		stop:  make(chan struct{}),
	}
	go func() {
		defer close(s.lines)
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				select {
				case s.lines <- line:
				case <-s.stop:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(s.close)
	return s
}

func (s *sseStream) close() {
	s.once.Do(func() {
		close(s.stop)           // release a reader blocked on send
		_ = s.resp.Body.Close() // release a reader blocked on read
	})
}

// waitForHeartbeat blocks until a heartbeat comment frame (": heartbeat") is
// read, proving the handler reached its serve loop and bytes are flowing (a
// stronger "connected" signal than the initial 200).
func (s *sseStream) waitForHeartbeat(deadline time.Duration) {
	s.t.Helper()
	timeout := time.After(deadline)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				s.t.Fatalf("SSE stream closed before any heartbeat")
			}
			if strings.HasPrefix(line, ":") {
				return
			}
		case <-timeout:
			s.t.Fatalf("timed out after %s waiting for SSE heartbeat", deadline)
		}
	}
}

// waitForSubject blocks until a data frame whose JSON payload carries the given
// subject is read, proving an event broadcast reached this client.
func (s *sseStream) waitForSubject(subject string, deadline time.Duration) {
	s.t.Helper()
	timeout := time.After(deadline)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				s.t.Fatalf("SSE stream closed before receiving subject %q", subject)
			}
			if data, found := strings.CutPrefix(strings.TrimSpace(line), "data: "); found {
				var ev struct {
					Subject string `json:"subject"`
				}
				if json.Unmarshal([]byte(data), &ev) == nil && ev.Subject == subject {
					return
				}
			}
		case <-timeout:
			s.t.Fatalf("timed out after %s waiting for SSE event with subject %q", deadline, subject)
		}
	}
}

// waitClosed drains the stream until it ends (channel closed), asserting the
// server released this client's handler within deadline. Without the drain
// wiring the superseded/shutting-down sender never unblocks the handler, so the
// stream stays open and this fails.
func (s *sseStream) waitClosed(deadline time.Duration) {
	s.t.Helper()
	timeout := time.After(deadline)
	for {
		select {
		case _, ok := <-s.lines:
			if !ok {
				return
			}
		case <-timeout:
			s.t.Fatalf("SSE stream did not close within %s (sender was not drained)", deadline)
		}
	}
}

// postEvent POSTs a message through the HTTP receiver, asserting 200.
func postEvent(t *testing.T, url, subject string) {
	t.Helper()
	body := strings.NewReader(fmt.Sprintf(`{"subject":%q,"payload":{"n":1}}`, subject))
	resp, err := http.Post(url, "application/json", body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestApp_StopDoesNotStallOnConnectedSSEClient is the primary C18-finding-1
// regression: with a live SSE subscriber, App.Stop must NOT block waiting on the
// SSE handler. Stop is called with a context budget far below the 30s shutdown
// default; the fix drains the sender (unblocking the handler) BEFORE
// transportServer.Stop -> server.Shutdown, so Stop returns promptly and cleanly.
// Without the drain wiring, server.Shutdown would block on the open SSE stream
// until this ctx deadline (~8s) and then return a deadline error — failing both
// the timing and the NoError assertion.
func TestApp_StopDoesNotStallOnConnectedSSEClient(t *testing.T) {
	app := startAppWithSSE(t, "bridge-stop")

	clientCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream := dialSSE(t, clientCtx, app.TransportURL()+"/events")
	// Prove the handler is parked in its serve loop before we Stop.
	stream.waitForHeartbeat(3 * time.Second)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer stopCancel()

	start := time.Now()
	err := app.Stop(stopCtx)
	elapsed := time.Since(start)

	require.NoError(t, err, "Stop must complete cleanly, not time out on the SSE handler")
	require.Less(t, elapsed, 3*time.Second,
		"App.Stop stalled on the connected SSE client (elapsed %s); the SSE drain is not wired", elapsed)

	// The subscriber's blocking read must unblock promptly (stream ended).
	stream.waitClosed(3 * time.Second)
}

// TestApp_ReloadKeepsSSEFlowingAndReleasesOldSender is the second
// C18-finding-1 regression: a hot reload must NOT leave subscribers pinned to
// the orphaned old sender (which would keep sending heartbeats but never events,
// and never reconnect because the socket looks alive) while the new sender
// broadcasts to nobody. The fix drains the superseded registry's SSE senders on
// the overlap swap's success tail, so the old client disconnects and a fresh
// client connects to the newly installed sender and receives events.
func TestApp_ReloadKeepsSSEFlowingAndReleasesOldSender(t *testing.T) {
	app := startAppWithSSE(t, "bridge-reload")

	// Subscriber on the ORIGINAL sender; verify end-to-end event flow.
	oldCtx, oldCancel := context.WithCancel(t.Context())
	defer oldCancel()
	oldClient := dialSSE(t, oldCtx, app.TransportURL()+"/events")
	oldClient.waitForHeartbeat(3 * time.Second)
	postEvent(t, app.TransportURL()+"/ingress", "before.reload")
	oldClient.waitForSubject("before.reload", 5*time.Second)

	// Hot reload: same SSE route, trivial change (log level). Overlap swap
	// builds and installs a FRESH registry + sender; the old one is superseded.
	reloadConfig(t, app, sseRouteConfig("bridge-reload", "debug"))

	// (a) The OLD subscriber's stream is drained/closed — it will reconnect
	//     instead of silently hanging on the orphaned sender.
	oldClient.waitClosed(5 * time.Second)

	// (b) A subscriber connecting AFTER the reload reaches the NEW sender and
	//     receives events broadcast through the freshly installed mux — proving
	//     the swap did not blackhole egress.
	newCtx, newCancel := context.WithCancel(t.Context())
	defer newCancel()
	newClient := dialSSE(t, newCtx, app.TransportURL()+"/events")
	newClient.waitForHeartbeat(3 * time.Second)
	postEvent(t, app.TransportURL()+"/ingress", "after.reload")
	newClient.waitForSubject("after.reload", 5*time.Second)
}
