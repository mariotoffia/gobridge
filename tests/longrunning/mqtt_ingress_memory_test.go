//go:build longrunning

package longrunning_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	runtimeroute "github.com/mariotoffia/gobridge/runtime/route"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

func TestMQTTIngressMemory(t *testing.T) {
	const (
		maxPayloadBytes  = 256 << 10
		receiveMaximum   = 16
		routeMaxInFlight = 4
		topic            = "longrunning/ingress-memory"
	)

	memoryLimit, limitSource, err := reliableProcessMemoryLimitBytes()
	if err != nil {
		memoryProofUnavailable(t, "reliable configured process/container memory limit unavailable: %v", err)
	}
	ingressBound, err := paho.IngressMemoryBound(maxPayloadBytes, receiveMaximum, routeMaxInFlight)
	require.NoError(t, err)
	if ingressBound > memoryLimit/4 {
		memoryProofUnavailable(t,
			"configured memory limit from %s is too small for measured profile: ingress bound %d > 25%% allocation %d",
			limitSource, ingressBound, memoryLimit/4)
	}
	brokerURL := os.Getenv("MQTT_MEMORY_BROKER_URL")
	if brokerURL == "" {
		if os.Getenv("GOBRIDGE_REQUIRE_MEMORY_LIMIT") == "1" {
			t.Fatal("required memory proof must provide MQTT_MEMORY_BROKER_URL for the externally managed CI broker")
		}
		broker := mqttlocal.NewBrokerInstance(t,
			mqttlocal.WithMaxInflightMessages(receiveMaximum),
			mqttlocal.WithMessageSizeLimit(maxPayloadBytes),
		)
		brokerURL = broker.URL()
	}

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)

	source := paho.NewSession(paho.SessionOptions{
		BrokerURLs:               []string{brokerURL},
		ClientID:                 mqttlocal.UniqueClientID("ingress-memory-source"),
		KeepAlive:                30,
		ConnectTimeout:           15 * time.Second,
		CleanStart:               true,
		ReceiveMaximum:           receiveMaximum,
		MaxPayloadBytes:          maxPayloadBytes,
		IngressMemoryBudgetBytes: ingressBound,
	}, connectivity.SessionEphemeral, testLogger(t))
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	require.NoError(t, source.Start(ctx))
	require.NoError(t, source.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	require.Eventually(t, func() bool {
		return source.Health(ctx).HasTopic(topic)
	}, 15*time.Second, 10*time.Millisecond)

	receiver := paho.NewReceiver("ingress-memory-receiver", source, paho.WithTopicFilters(topic))
	outbox := newBlockingMemoryProofOutbox(routeMaxInFlight)
	runner := runtimeroute.NewRouteRunnerFromConfig(runtimeroute.RouteRunnerConfig{
		RouteID: "ingress-memory-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliverySharedOutbox,
			MaxInFlight:        routeMaxInFlight,
			MaxReplayAttempts:  1,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		Receiver:    receiver,
		OutboxStore: outbox,
		Bindings: []routing.DestinationBinding{{
			ID:        "memory-binding",
			SessionID: "memory-egress",
			Address:   "memory/out",
		}},
	})
	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		require.Eventually(t, func() bool {
			select {
			case <-runnerDone:
				return true
			default:
				return false
			}
		}, 10*time.Second, 10*time.Millisecond, "MQTT ingress memory route did not stop")
	})
	select {
	case <-receiver.Started():
	case <-ctx.Done():
		t.Fatal("MQTT ingress memory receiver did not start")
	}

	publisher, err := startPublisherHelper(ctx, brokerURL, topic, maxPayloadBytes)
	require.NoError(t, err)
	t.Cleanup(func() { _ = publisher.Close() })

	require.NoError(t, publisher.Publish(1, 1))
	select {
	case <-outbox.warmed:
	case <-ctx.Done():
		t.Fatal("shared-outbox warm-up did not persist")
	}
	require.Eventually(t, func() bool {
		return source.Health(ctx).UnsettledCount == 0
	}, 15*time.Second, 10*time.Millisecond, "warm-up publish did not settle")
	require.GreaterOrEqual(t, outbox.warmHeaders.Load(), int64(125),
		"warm-up must prove the worst accepted User Property set reached shared outbox")
	outbox.armed.Store(true)
	// Collect the completed warm-up objects while retaining Go's already-faulted
	// heap pages. This establishes a production-like steady-state baseline;
	// debug.FreeOSMemory would forcibly scavenge reusable spans and charge normal
	// allocator reuse to ingress as if it were retained message memory.
	runtime.GC()

	sampler, err := startMemorySampler(ctx, memoryCurrentPath(limitSource), 10*time.Millisecond)
	if err != nil {
		memoryProofUnavailable(t, "reliable continuous cgroup memory sampling unavailable: %v", err)
	}
	t.Cleanup(func() { _, _ = sampler.Stop() })

	require.NoError(t, publisher.Publish(1, receiveMaximum))
	for range routeMaxInFlight {
		select {
		case <-outbox.entered:
		case <-ctx.Done():
			t.Fatal("shared-outbox persist barrier did not fill")
		}
	}
	require.Equal(t, int64(routeMaxInFlight), runner.InFlight(),
		"exactly the modeled route window must remain blocked in outbox persist")
	require.Equal(t, int64(routeMaxInFlight), outbox.retained.Load())

	require.Eventually(t, func() bool {
		return source.Health(ctx).UnsettledCount == receiveMaximum
	}, 15*time.Second, 10*time.Millisecond,
		"QoS 1 publishes must fill the broker receive window")
	require.Eventually(t, func() bool {
		depth, _ := source.IngressMemoryStats()
		return depth == receiveMaximum-routeMaxInFlight-1
	}, 15*time.Second, 10*time.Millisecond,
		"receive window must split exactly across route, crossing, and dispatch ownership")

	qos1DispatchDepth, dispatchCapacity := source.IngressMemoryStats()
	require.Less(t, qos1DispatchDepth, dispatchCapacity)
	require.NoError(t, publisher.Publish(0, dispatchCapacity-qos1DispatchDepth))
	require.Eventually(t, func() bool {
		depth, capacity := source.IngressMemoryStats()
		return capacity == dispatchCapacity && depth == capacity
	}, 15*time.Second, 10*time.Millisecond,
		"QoS 0 publishes must fill the adapter dispatch queue while downstream is blocked")

	peakMemory, sampleErr := sampler.Stop()
	require.NoError(t, sampleErr)
	require.GreaterOrEqual(t, sampler.Samples(), uint64(3),
		"memory sampler must observe baseline, at least one fill interval, and final full-window state")
	baselineMemory := sampler.Baseline()
	require.GreaterOrEqual(t, peakMemory, baselineMemory)
	memoryDelta := peakMemory - baselineMemory
	modeledPayloadBytes := uint64(maxPayloadBytes) *
		uint64(receiveMaximum+dispatchCapacity+routeMaxInFlight+1)
	allocatorTolerance := modeledPayloadBytes / 4
	require.GreaterOrEqual(t, memoryDelta+allocatorTolerance, modeledPayloadBytes,
		"cgroup memory delta %d did not prove retention of modeled payload window %d (tolerance %d)",
		memoryDelta, modeledPayloadBytes, allocatorTolerance)
	require.LessOrEqual(t, memoryDelta, ingressBound,
		"cgroup memory delta %d exceeds configured ingress budget %d", memoryDelta, ingressBound)

	minimumHeadroom := memoryLimit / 5
	if memoryLimit%5 != 0 {
		minimumHeadroom++
	}
	require.Less(t, peakMemory, memoryLimit-minimumHeadroom,
		"peak cgroup memory.current %d must stay below 80%% of configured limit %d from %s",
		peakMemory, memoryLimit, limitSource)
}

type blockingMemoryProofOutbox struct {
	entered     chan struct{}
	warmed      chan struct{}
	warmOnce    sync.Once
	armed       atomic.Bool
	warmHeaders atomic.Int64
	retained    atomic.Int64
}

func newBlockingMemoryProofOutbox(capacity int) *blockingMemoryProofOutbox {
	return &blockingMemoryProofOutbox{
		entered: make(chan struct{}, capacity),
		warmed:  make(chan struct{}),
	}
}

func (s *blockingMemoryProofOutbox) Persist(ctx context.Context, records []*persistence.OutboxRecord) error {
	if !s.armed.Load() {
		if len(records) > 0 {
			s.warmHeaders.Store(int64(len(records[0].Snapshot().Headers())))
		}
		s.warmOnce.Do(func() { close(s.warmed) })
		return nil
	}
	s.retained.Add(int64(len(records)))
	select {
	case s.entered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}

func (*blockingMemoryProofOutbox) Claim(
	context.Context,
	string,
	persistence.LeaseToken,
	int,
) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (*blockingMemoryProofOutbox) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

func (*blockingMemoryProofOutbox) Expire(context.Context, time.Time, string) (int, error) {
	return 0, nil
}

func (*blockingMemoryProofOutbox) QueryPending(
	context.Context,
	string,
	int,
) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func memoryProofUnavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("GOBRIDGE_REQUIRE_MEMORY_LIMIT") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

func currentMemoryBytes(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read cgroup memory current %s: %w", path, err)
	}
	current, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse cgroup memory current %q: %w", strings.TrimSpace(string(data)), err)
	}
	return current, nil
}

type publisherHelper struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	conn    net.Conn
	output  *bufio.Scanner
	stderr  strings.Builder
	mu      sync.Mutex
	stopped bool
}

func startPublisherHelper(
	ctx context.Context,
	brokerURL string,
	topic string,
	payloadBytes int,
) (*publisherHelper, error) {
	if controlAddress := os.Getenv("MQTT_MEMORY_PUBLISHER_CONTROL"); controlAddress != "" {
		conn, err := dialPublisherControl(ctx, controlAddress)
		if err != nil {
			return nil, err
		}
		return &publisherHelper{
			stdin:  conn,
			conn:   conn,
			output: bufio.NewScanner(conn),
		}, nil
	}

	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve test executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, executable,
		"-test.run", "^TestMQTTIngressMemoryPublisherProcess$",
		"-test.count=1",
	)
	cmd.Env = append(os.Environ(),
		"GOBRIDGE_MQTT_MEMORY_PUBLISHER_HELPER=1",
		"GOBRIDGE_MQTT_MEMORY_BROKER_URL="+brokerURL,
		"GOBRIDGE_MQTT_MEMORY_TOPIC="+topic,
		"GOBRIDGE_MQTT_MEMORY_PAYLOAD_BYTES="+strconv.Itoa(payloadBytes),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("publisher stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("publisher stdout: %w", err)
	}
	helper := &publisherHelper{
		cmd:    cmd,
		stdin:  stdin,
		output: bufio.NewScanner(stdout),
	}
	cmd.Stderr = &helper.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start MQTT publisher helper: %w", err)
	}
	if !helper.output.Scan() || helper.output.Text() != "READY" {
		_ = helper.Close()
		return nil, fmt.Errorf("publisher helper did not become ready: %s", helper.stderr.String())
	}
	return helper, nil
}

func (p *publisherHelper) Publish(qos, count int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return fmt.Errorf("publisher helper is stopped")
	}
	if _, err := fmt.Fprintf(p.stdin, "%d %d\n", qos, count); err != nil {
		return fmt.Errorf("command publisher helper: %w", err)
	}
	if !p.output.Scan() || p.output.Text() != "DONE" {
		return fmt.Errorf("publisher helper failed: %s", p.stderr.String())
	}
	return nil
}

func (p *publisherHelper) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return nil
	}
	p.stopped = true
	if p.conn != nil {
		return p.conn.Close()
	}
	_ = p.stdin.Close()
	if err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("wait publisher helper: %w (%s)", err, p.stderr.String())
	}
	return nil
}

func dialPublisherControl(ctx context.Context, address string) (net.Conn, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect external publisher helper %s: %w", address, ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestMQTTIngressMemoryPublisherProcess(t *testing.T) {
	if os.Getenv("GOBRIDGE_MQTT_MEMORY_PUBLISHER_HELPER") != "1" {
		t.Skip("subprocess-only MQTT ingress memory publisher")
	}
	payloadBytes, err := strconv.Atoi(os.Getenv("GOBRIDGE_MQTT_MEMORY_PAYLOAD_BYTES"))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	session := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{os.Getenv("GOBRIDGE_MQTT_MEMORY_BROKER_URL")},
		ClientID:       mqttlocal.UniqueClientID("ingress-memory-publisher"),
		KeepAlive:      30,
		ConnectTimeout: 15 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	require.NoError(t, session.Start(ctx))

	headers := make(map[string]any, 127)
	value := strings.Repeat("v", 256)
	for i := range 125 {
		key := fmt.Sprintf("proof-%03d", i)
		headers[key] = value
	}
	// Two legal MQTT UTF-8 values drive encoded metadata close to the accepted
	// 128 KiB ceiling while 125 safe headers plus the generated message ID drive
	// the User Property count to its exact cap of 128. The large values are
	// intentionally dropped by Envelope header hygiene after admission, but
	// remain retained in Paho's unsettled wire packets during the measurement.
	headers["proof-filler-a"] = strings.Repeat("a", 46_000)
	headers["proof-filler-b"] = strings.Repeat("b", 46_000)
	message := ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
			Payload: make([]byte, payloadBytes),
			Headers: headers,
		}),
		Address: os.Getenv("GOBRIDGE_MQTT_MEMORY_TOPIC"),
	}
	input := io.Reader(os.Stdin)
	output := io.Writer(os.Stdout)
	var listener net.Listener
	if address := os.Getenv("GOBRIDGE_MQTT_MEMORY_PUBLISHER_LISTEN"); address != "" {
		listener, err = net.Listen("tcp", address)
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })
		conn, acceptErr := listener.Accept()
		require.NoError(t, acceptErr)
		t.Cleanup(func() { _ = conn.Close() })
		input = conn
		output = conn
	} else {
		fmt.Fprintln(output, "READY")
	}
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		var qos, count int
		_, err := fmt.Sscanf(scanner.Text(), "%d %d", &qos, &count)
		require.NoError(t, err)
		sender := paho.NewSender(session, paho.SenderOptions{
			QoS:     byte(qos),
			Timeout: 30 * time.Second,
		})
		for range count {
			require.NoError(t, sender.Send(ctx, message))
		}
		fmt.Fprintln(output, "DONE")
	}
	require.NoError(t, scanner.Err())
}

func reliableProcessMemoryLimitBytes() (uint64, string, error) {
	if runtime.GOOS != "linux" {
		return 0, "", fmt.Errorf("cgroup memory limits are only measurable on linux (running %s)", runtime.GOOS)
	}
	candidates := []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	}
	reasons := make([]string, 0, len(candidates))
	for _, path := range candidates {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			reasons = append(reasons, path+": "+readErr.Error())
			continue
		}
		raw := strings.TrimSpace(string(data))
		if raw == "" || raw == "max" {
			reasons = append(reasons, path+": unlimited")
			continue
		}
		limit, parseErr := strconv.ParseUint(raw, 10, 64)
		if parseErr != nil {
			reasons = append(reasons, path+": "+parseErr.Error())
			continue
		}
		// cgroup v1 represents "unlimited" as a value near MaxInt64.
		if limit == 0 || limit >= 1<<60 {
			reasons = append(reasons, fmt.Sprintf("%s: non-finite limit %d", path, limit))
			continue
		}
		return limit, path, nil
	}
	return 0, "", fmt.Errorf("%s", strings.Join(reasons, "; "))
}

func memoryCurrentPath(limitPath string) string {
	if strings.HasSuffix(limitPath, "memory.max") {
		return strings.TrimSuffix(limitPath, "memory.max") + "memory.current"
	}
	return strings.TrimSuffix(limitPath, "memory.limit_in_bytes") + "memory.usage_in_bytes"
}

type memorySampler struct {
	baseline uint64
	peak     atomic.Uint64
	samples  atomic.Uint64
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	err      error
}

func startMemorySampler(parent context.Context, currentPath string, interval time.Duration) (*memorySampler, error) {
	baseline, err := currentMemoryBytes(currentPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	sampler := &memorySampler{
		baseline: baseline,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	sampler.peak.Store(baseline)
	sampler.samples.Store(1)
	go sampler.run(ctx, currentPath, interval)
	return sampler, nil
}

func (s *memorySampler) run(ctx context.Context, currentPath string, interval time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.sample(currentPath)
			return
		case <-ticker.C:
			if !s.sample(currentPath) {
				return
			}
		}
	}
}

func (s *memorySampler) sample(currentPath string) bool {
	rss, err := currentMemoryBytes(currentPath)
	if err != nil {
		s.err = err
		return false
	}
	s.samples.Add(1)
	for {
		peak := s.peak.Load()
		if rss <= peak || s.peak.CompareAndSwap(peak, rss) {
			return true
		}
	}
}

func (s *memorySampler) Stop() (uint64, error) {
	s.stopOnce.Do(s.cancel)
	<-s.done
	return s.peak.Load(), s.err
}

func (s *memorySampler) Baseline() uint64 {
	return s.baseline
}

func (s *memorySampler) Samples() uint64 {
	return s.samples.Load()
}
