//go:build longrunning

package longrunning_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

func TestMQTTIngressMemory_PeakRSSBelowContainerLimit(t *testing.T) {
	const (
		maxPayloadBytes  = 256 << 10
		receiveMaximum   = 16
		routeMaxInFlight = 4
		topic            = "longrunning/ingress-memory"
	)

	memoryLimit, limitSource, err := reliableProcessMemoryLimitBytes()
	if err != nil {
		t.Skipf("reliable configured process/container memory limit unavailable: %v", err)
	}
	ingressBound, err := paho.IngressMemoryBound(maxPayloadBytes, receiveMaximum, routeMaxInFlight)
	require.NoError(t, err)
	if ingressBound > memoryLimit/4 {
		t.Skipf("configured memory limit from %s is too small for measured profile: ingress bound %d > 25%% allocation %d",
			limitSource, ingressBound, memoryLimit/4)
	}

	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithMaxInflightMessages(receiveMaximum),
		mqttlocal.WithMessageSizeLimit(maxPayloadBytes),
	)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)

	source := paho.NewSession(paho.SessionOptions{
		BrokerURLs:               []string{broker.URL()},
		ClientID:                 mqttlocal.UniqueClientID("ingress-memory-source"),
		KeepAlive:                30,
		ConnectTimeout:           15 * time.Second,
		CleanStart:               true,
		ReceiveMaximum:           receiveMaximum,
		MaxPayloadBytes:          maxPayloadBytes,
		IngressMemoryBudgetBytes: memoryLimit / 4,
	}, connectivity.SessionEphemeral, testLogger(t))
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	require.NoError(t, source.Start(ctx))
	require.NoError(t, source.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	require.Eventually(t, func() bool {
		return source.Health(ctx).HasTopic(topic)
	}, 15*time.Second, 10*time.Millisecond)

	held := make(chan ports.Delivery, routeMaxInFlight)
	accepted := make(chan struct{}, routeMaxInFlight)
	blocked := make(chan struct{})
	var acceptedCount atomic.Int64
	var blockedOnce sync.Once
	receiver := paho.NewReceiver("ingress-memory-receiver", source, paho.WithTopicFilters(topic))
	receiverDone := make(chan error, 1)
	go func() {
		receiverDone <- receiver.Run(ctx, func(emitCtx context.Context, delivery ports.Delivery) error {
			select {
			case held <- delivery:
				acceptedCount.Add(1)
				accepted <- struct{}{}
				return nil
			default:
				blockedOnce.Do(func() { close(blocked) })
			}
			select {
			case held <- delivery:
				acceptedCount.Add(1)
				return nil
			case <-emitCtx.Done():
				return emitCtx.Err()
			}
		})
	}()
	t.Cleanup(func() {
		cancel()
		require.Eventually(t, func() bool {
			select {
			case <-receiverDone:
				return true
			default:
				return false
			}
		}, 10*time.Second, 10*time.Millisecond, "MQTT ingress memory receiver did not stop")
	})
	select {
	case <-receiver.Started():
	case <-ctx.Done():
		t.Fatal("MQTT ingress memory receiver did not start")
	}

	debug.FreeOSMemory()
	sampler, err := startRSSSampler(ctx, 10*time.Millisecond)
	if err != nil {
		t.Skipf("reliable continuous RSS sampling unavailable: %v", err)
	}
	t.Cleanup(func() { _, _ = sampler.Stop() })

	payload := make([]byte, maxPayloadBytes)

	publishStarted := make(chan struct{}, receiveMaximum)
	var publishers sync.WaitGroup
	publishers.Add(receiveMaximum)
	for range receiveMaximum {
		go func() {
			defer publishers.Done()
			_ = publishFromBrokerContainer(
				ctx, broker.ContainerName(), topic, 1, payload, publishStarted,
			)
		}()
	}
	t.Cleanup(func() {
		cancel()
		publishers.Wait()
	})
	for range receiveMaximum {
		select {
		case <-publishStarted:
		case <-ctx.Done():
			t.Fatal("QoS 1 publisher did not start")
		}
	}
	for range routeMaxInFlight {
		select {
		case <-accepted:
		case <-ctx.Done():
			t.Fatal("route in-flight barrier did not fill")
		}
	}
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("downstream blocked barrier was not reached")
	}
	require.Equal(t, int64(routeMaxInFlight), acceptedCount.Load(),
		"exactly the modeled route window must remain accepted while the next delivery blocks")

	require.Eventually(t, func() bool {
		return source.Health(ctx).UnsettledCount == receiveMaximum
	}, 15*time.Second, 10*time.Millisecond,
		"QoS 1 publishes must fill the broker receive window")

	_, dispatchCapacity := source.IngressMemoryStats()
	for range dispatchCapacity + 1 {
		require.NoError(t, publishFromBrokerContainer(
			ctx, broker.ContainerName(), topic, 0, payload, nil,
		))
	}
	require.Eventually(t, func() bool {
		depth, capacity := source.IngressMemoryStats()
		return capacity == dispatchCapacity && depth == capacity
	}, 15*time.Second, 10*time.Millisecond,
		"QoS 0 publishes must fill the adapter dispatch queue while downstream is blocked")

	peakRSS, sampleErr := sampler.Stop()
	require.NoError(t, sampleErr)
	require.GreaterOrEqual(t, sampler.Samples(), uint64(3),
		"RSS sampler must observe baseline, at least one fill interval, and final full-window state")
	baselineRSS := sampler.Baseline()
	require.GreaterOrEqual(t, peakRSS, baselineRSS)
	rssDelta := peakRSS - baselineRSS
	modeledPayloadBytes := uint64(maxPayloadBytes) *
		uint64(receiveMaximum+dispatchCapacity+routeMaxInFlight+1)
	allocatorTolerance := modeledPayloadBytes / 4
	require.GreaterOrEqual(t, rssDelta+allocatorTolerance, modeledPayloadBytes,
		"RSS delta %d did not prove retention of modeled payload window %d (tolerance %d)",
		rssDelta, modeledPayloadBytes, allocatorTolerance)

	minimumHeadroom := memoryLimit / 5
	if memoryLimit%5 != 0 {
		minimumHeadroom++
	}
	require.Less(t, peakRSS, memoryLimit-minimumHeadroom,
		"peak RSS %d must stay below 80%% of configured limit %d from %s",
		peakRSS, memoryLimit, limitSource)
}

func currentRSSBytes() (uint64, error) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, fmt.Errorf("read /proc/self/statm: %w", err)
	}

	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected /proc/self/statm content %q", strings.TrimSpace(string(data)))
	}
	residentPages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse resident pages %q: %w", fields[1], err)
	}
	pageSize := uint64(os.Getpagesize())
	if residentPages > ^uint64(0)/pageSize {
		return 0, fmt.Errorf("resident page count %d overflows bytes", residentPages)
	}
	return residentPages * pageSize, nil
}

func publishFromBrokerContainer(
	ctx context.Context,
	containerName string,
	topic string,
	qos int,
	payload []byte,
	started chan<- struct{},
) error {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("find docker: %w", err)
	}
	cmd := exec.CommandContext(ctx, docker,
		"exec", "-i", containerName,
		"mosquitto_pub",
		"-h", "127.0.0.1",
		"-p", "1883",
		"-t", topic,
		"-q", strconv.Itoa(qos),
		"-s",
	)
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mosquitto_pub QoS %d: %w", qos, err)
	}
	if started != nil {
		started <- struct{}{}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("mosquitto_pub QoS %d: %w (%s)", qos, err, strings.TrimSpace(stderr.String()))
	}
	return nil
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

type rssSampler struct {
	baseline uint64
	peak     atomic.Uint64
	samples  atomic.Uint64
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	err      error
}

func startRSSSampler(parent context.Context, interval time.Duration) (*rssSampler, error) {
	baseline, err := currentRSSBytes()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	sampler := &rssSampler{
		baseline: baseline,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	sampler.peak.Store(baseline)
	sampler.samples.Store(1)
	go sampler.run(ctx, interval)
	return sampler, nil
}

func (s *rssSampler) run(ctx context.Context, interval time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.sample()
			return
		case <-ticker.C:
			if !s.sample() {
				return
			}
		}
	}
}

func (s *rssSampler) sample() bool {
	rss, err := currentRSSBytes()
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

func (s *rssSampler) Stop() (uint64, error) {
	s.stopOnce.Do(s.cancel)
	<-s.done
	return s.peak.Load(), s.err
}

func (s *rssSampler) Baseline() uint64 {
	return s.baseline
}

func (s *rssSampler) Samples() uint64 {
	return s.samples.Load()
}
