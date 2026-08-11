package paho

import (
	"math"
	"strings"
	"sync"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func TestConfigIngressMemory_DefaultsNormalize(t *testing.T) {
	cfg := DefaultConfig()

	assert.Zero(t, cfg.Session.MaxPayloadBytes)
	assert.Zero(t, cfg.Session.ReceiveMaximum)
	assert.Zero(t, cfg.Session.IngressMemoryBudgetBytes,
		"parsed defaults stay unset until deployment preflight/runtime normalization")

	session := NewSession(SessionOptions{}, connectivity.SessionEphemeral, nil)
	dispatchDepth, dispatchCapacity := session.IngressMemoryStats()
	assert.Zero(t, dispatchDepth)
	assert.Equal(t, int(DefaultReceiveMaximum), dispatchCapacity)
	assert.Equal(t, DefaultMaxPayloadBytes, session.opts.MaxPayloadBytes)
	assert.Equal(t, DefaultIngressMemoryBudgetBytes, session.opts.IngressMemoryBudgetBytes)
}

func TestConfigIngressMemory_OmittedReceiveMaximumDefersWindowValidation(t *testing.T) {
	cfg := decodeRegistry(t, map[string]any{
		"session": map[string]any{
			"max_payload_bytes": 1 << 20,
		},
	})

	require.Zero(t, cfg.Session.ReceiveMaximum)
	require.NoError(t, cfg.Validate(),
		"parse-time validation must leave omitted receive concurrency for deployment preflight")
	require.Error(t, cfg.ValidateIngressMemory(0),
		"full generic preflight must apply Receive Maximum 192 and reject the unsafe window")
}

func TestConfigIngressMemory_ExplicitReceiveMaximumRunsFullParseValidation(t *testing.T) {
	reg := ports.NewRegistry()
	require.NoError(t, Register(reg))

	_, err := reg.Decode("mqtt", parser.NewRawConfig(map[string]any{
		"session": map[string]any{
			"max_payload_bytes": 1 << 20,
			"receive_maximum":   192,
		},
	}))
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func TestFactoryIngressMemory_OmittedReceiveMaximumRunsFullBuildValidation(t *testing.T) {
	cfg := decodeRegistry(t, map[string]any{
		"session": map[string]any{
			"broker_url":        "tcp://broker:1883",
			"client_id":         "generic-build",
			"max_payload_bytes": 1 << 20,
		},
	})

	_, err := NewFactory(nil).NewSession(t.Context(), ports.SessionSpec{
		ID:     "generic-build",
		Config: cfg,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func TestConfigIngressMemory_ExplicitZeroRemainsUnsetUntilRuntimeNormalization(t *testing.T) {
	cfg := decodeRegistry(t, map[string]any{
		"session": map[string]any{
			"receive_maximum":             0,
			"max_payload_bytes":           0,
			"ingress_memory_budget_bytes": 0,
		},
	})

	assert.Zero(t, cfg.Session.ReceiveMaximum)
	assert.Zero(t, cfg.Session.MaxPayloadBytes)
	assert.Zero(t, cfg.Session.IngressMemoryBudgetBytes)

	session := NewSession(cfg.Session, connectivity.SessionEphemeral, nil)
	assert.Equal(t, DefaultReceiveMaximum, session.opts.ReceiveMaximum)
	assert.Equal(t, DefaultMaxPayloadBytes, session.opts.MaxPayloadBytes)
	assert.Equal(t, DefaultIngressMemoryBudgetBytes, session.opts.IngressMemoryBudgetBytes)
}

func TestConfigIngressMemory_ExactBoundaryAcceptsAndOneByteExcessRejects(t *testing.T) {
	const routeMaxInFlight uint64 = 100
	bound, err := IngressMemoryBound(DefaultMaxPayloadBytes, DefaultReceiveMaximum, routeMaxInFlight)
	require.NoError(t, err)

	cfg := Config{Session: SessionOptions{
		MaxPayloadBytes:          DefaultMaxPayloadBytes,
		ReceiveMaximum:           DefaultReceiveMaximum,
		IngressMemoryBudgetBytes: bound,
	}}
	require.NoError(t, cfg.ValidateIngressMemory(routeMaxInFlight))

	cfg.Session.IngressMemoryBudgetBytes = bound - 1
	err = cfg.ValidateIngressMemory(routeMaxInFlight)
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func TestConfigIngressMemory_RouteConcurrencyContributesToBound(t *testing.T) {
	withoutRoute, err := IngressMemoryBound(DefaultMaxPayloadBytes, DefaultReceiveMaximum, 0)
	require.NoError(t, err)
	withRoute, err := IngressMemoryBound(DefaultMaxPayloadBytes, DefaultReceiveMaximum, 37)
	require.NoError(t, err)
	packet, err := ingressMemoryPacketBytes(DefaultMaxPayloadBytes)
	require.NoError(t, err)

	assert.Equal(t, packet*37, withRoute-withoutRoute)
}

func TestConfigIngressMemory_TooSmallForOnePacketRejects(t *testing.T) {
	packet, err := ingressMemoryPacketBytes(DefaultMaxPayloadBytes)
	require.NoError(t, err)

	_, err = LargestSafeReceiveMaximum(DefaultMaxPayloadBytes, packet-1, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func TestConfigIngressMemory_MaxIntegerOverflowRejects(t *testing.T) {
	tests := []struct {
		name             string
		routeMaxInFlight uint64
	}{
		{name: "window addition", routeMaxInFlight: math.MaxUint64},
		{name: "bound multiplication", routeMaxInFlight: math.MaxUint64 / 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := IngressMemoryBound(DefaultMaxPayloadBytes, math.MaxUint16, test.routeMaxInFlight)
			require.Error(t, err)
			assert.ErrorIs(t, err, shared.ErrInvalidConfig)
		})
	}
}

func TestRouterIngressMemory_OversizePayloadAckDroppedWithoutTerminal(t *testing.T) {
	rec := &ports.RecordingExporter{}
	session := NewSession(SessionOptions{
		MaxPayloadBytes: 4,
		ReceiveMaximum:  2,
	}, connectivity.SessionEphemeral, nil, rec)
	session.router.beginGrace()

	handled, err := session.router.onPublishReceived(pahov5.PublishReceived{
		Packet: &pahov5.Publish{Topic: "memory/oversize", QoS: 1, Payload: []byte("12345")},
	})
	require.NoError(t, err)
	assert.True(t, handled)

	dispatchDepth, dispatchCapacity := session.IngressMemoryStats()
	assert.Zero(t, dispatchDepth)
	assert.Equal(t, 2, dispatchCapacity)
	assert.Zero(t, session.router.PendingCount())
	assert.Zero(t, session.Health(t.Context()).UnsettledCount,
		"a poison drop must not create an unfinishable unsettled tracker entry")
	assert.Equal(t, int64(1), session.router.IngressPoisonDroppedCount())
	assert.Len(t, rec.FindEntries(MetricMQTTIngressPoisonDropped), 1)
	session.mu.Lock()
	terminalErr := session.terminalErr
	session.mu.Unlock()
	require.NoError(t, terminalErr,
		"a broker-forwardable cap violation must be acked-and-dropped, never terminal")
}

func TestRouterIngressMemory_MetadataCapAckDropsEveryPoisonBeforeEnqueue(t *testing.T) {
	rec := &ports.RecordingExporter{}
	session := NewSession(SessionOptions{
		MaxPayloadBytes: 16,
		ReceiveMaximum:  2,
	}, connectivity.SessionPersistent, nil, rec)
	session.router.beginGrace()
	properties := make(pahov5.UserProperties, maxIngressUserProperties+1)
	for i := range properties {
		properties[i] = pahov5.UserProperty{Key: "k", Value: "v"}
	}
	packet := &pahov5.Publish{
		Topic:      "memory/metadata-poison",
		QoS:        1,
		Payload:    []byte("ok"),
		Properties: &pahov5.PublishProperties{User: properties},
	}

	for range 2 {
		handled, err := session.router.onPublishReceived(pahov5.PublishReceived{Packet: packet})
		require.NoError(t, err)
		assert.True(t, handled)
	}

	assert.Zero(t, session.router.PendingCount())
	depth, _ := session.IngressMemoryStats()
	assert.Zero(t, depth)
	assert.Zero(t, session.Health(t.Context()).UnsettledCount)
	assert.Equal(t, int64(2), session.router.IngressPoisonDroppedCount(),
		"EVERY poison drop is counted; only the Error log is deduped per class")
	assert.Len(t, rec.FindEntries(MetricMQTTIngressPoisonDropped), 2)
	session.mu.Lock()
	terminalErr := session.terminalErr
	session.mu.Unlock()
	require.NoError(t, terminalErr, "poison must never latch terminal")
}

func TestRouterIngressMemory_MetadataExactBoundaryAcceptsAndOneByteExcessAckDrops(t *testing.T) {
	const topicBytes = 65535
	valueBytes := int(maxIngressMetadataBytes) - (1 + 4 + 2 + topicBytes + 2 + 4) - (5 + 1)
	require.Positive(t, valueBytes)
	properties := func(extra int) *pahov5.PublishProperties {
		return &pahov5.PublishProperties{User: pahov5.UserProperties{{
			Key: "k", Value: strings.Repeat("v", valueBytes+extra),
		}}}
	}

	accepted := NewSession(SessionOptions{
		MaxPayloadBytes: 16,
		ReceiveMaximum:  2,
	}, connectivity.SessionEphemeral, nil)
	packet := &pahov5.Publish{
		Topic: strings.Repeat("t", topicBytes), QoS: 1,
		Payload: []byte("ok"), Properties: properties(0),
	}
	require.Equal(t, maxIngressMetadataBytes, ingressMetadataBytes(packet))
	handled, err := accepted.router.onPublishReceived(pahov5.PublishReceived{Packet: packet})
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, 1, accepted.router.PendingCount())
	assert.Zero(t, accepted.router.IngressPoisonDroppedCount())
	accepted.mu.Lock()
	acceptedTerminal := accepted.terminalErr
	accepted.mu.Unlock()
	assert.NoError(t, acceptedTerminal)

	rejected := NewSession(SessionOptions{
		MaxPayloadBytes: 16,
		ReceiveMaximum:  2,
	}, connectivity.SessionEphemeral, nil)
	packet.Properties = properties(1)
	require.Equal(t, maxIngressMetadataBytes+1, ingressMetadataBytes(packet))
	handled, err = rejected.router.onPublishReceived(pahov5.PublishReceived{Packet: packet})
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Zero(t, rejected.router.PendingCount())
	assert.Equal(t, int64(1), rejected.router.IngressPoisonDroppedCount())
	rejected.mu.Lock()
	rejectedTerminal := rejected.terminalErr
	rejected.mu.Unlock()
	require.NoError(t, rejectedTerminal,
		"one byte over the metadata cap is broker-forwardable and must ack-drop, not terminate")
}

func TestRouterIngressMemory_AcceptedPacketRetainsImmutableCallbackBacking(t *testing.T) {
	session := NewSession(SessionOptions{
		MaxPayloadBytes: 16,
		ReceiveMaximum:  2,
	}, connectivity.SessionEphemeral, nil)
	packet := &pahov5.Publish{
		Topic:   "memory/ownership",
		QoS:     1,
		Payload: []byte("immutable"),
		Properties: &pahov5.PublishProperties{User: pahov5.UserProperties{{
			Key: HeaderMessageID, Value: "stable-id",
		}}},
	}

	handled, err := session.router.onPublishReceived(pahov5.PublishReceived{Packet: packet})
	require.NoError(t, err)
	assert.True(t, handled)

	session.router.mu.RLock()
	require.Len(t, session.router.pending, 1)
	retained := session.router.pending[0].pub
	session.router.mu.RUnlock()
	assert.Same(t, packet, retained)
	assert.Equal(t, &packet.Payload[0], &retained.Payload[0])
	assert.Same(t, packet.Properties, retained.Properties)
}

func TestRouterIngressMemory_PendingAndDispatchShareCapacity(t *testing.T) {
	router := newRouter(nil, nil, withDispatchCapacity(2), withMaxPayloadBytes(16))
	pendingQoS1 := &pahov5.Publish{Topic: "pending/1", QoS: 1}
	pendingQoS0 := &pahov5.Publish{Topic: "pending/2", QoS: 0}
	require.True(t, router.reserveQueueSlot(pendingQoS1, pendingQoS1.QoS))
	router.dispatch(pendingQoS1, nil)
	require.True(t, router.reserveQueueSlot(pendingQoS0, pendingQoS0.QoS))
	router.dispatch(pendingQoS0, nil)

	dispatchQoS0 := &pahov5.Publish{Topic: "dispatch/full", QoS: 0}
	assert.False(t, router.reserveQueueSlot(dispatchQoS0, dispatchQoS0.QoS),
		"dispatch must not reserve independently after pending owns all capacity")

	router.mu.RLock()
	assert.Len(t, router.pending, 2)
	assert.Equal(t, 2, router.queueReserved)
	assert.Len(t, router.queueReservations, 2)
	router.mu.RUnlock()
}

func TestRouterIngressMemory_ReservationReleaseIsIdempotentAcrossLifecyclePaths(t *testing.T) {
	t.Run("matching dispatch", func(t *testing.T) {
		router := newRouter(nil, nil, withDispatchCapacity(1))
		delivered := make(chan struct{}, 1)
		router.RegisterFiltered("receiver", []string{"match/#"}, func(*pahov5.Publish, func() error) {
			delivered <- struct{}{}
		})
		pub := &pahov5.Publish{Topic: "match/one", QoS: 1}
		require.True(t, router.reserveQueueSlot(pub, pub.QoS))
		router.dispatch(pub, nil)
		<-delivered
		router.releaseQueueReservation(pub)
		assertRouterReservations(t, router, 0)
	})

	t.Run("stale epoch purge", func(t *testing.T) {
		router := newRouter(nil, nil, withDispatchCapacity(1))
		pub := &pahov5.Publish{Topic: "pending/stale", QoS: 1}
		require.True(t, router.reserveQueueSlot(pub, pub.QoS))
		router.dispatch(pub, nil)

		router.mu.Lock()
		router.connEpoch++
		router.purgeStalePendingLocked()
		router.mu.Unlock()
		assertRouterReservations(t, router, 0)
		assert.Zero(t, router.PendingCount())
	})

	t.Run("managed migration pending", func(t *testing.T) {
		router := newRouter(nil, nil, withDispatchCapacity(1))
		router.mu.Lock()
		router.quiesced = true
		router.mu.Unlock()

		pub := &pahov5.Publish{Topic: "managed/replay", QoS: 1}
		require.True(t, router.reserveQueueSlot(pub, pub.QoS))
		router.dispatch(pub, nil)
		assertRouterReservations(t, router, 1)
		assert.Equal(t, 1, router.PendingCount())

		router.mu.Lock()
		router.quiesced = false
		router.mu.Unlock()
		delivered := make(chan struct{}, 1)
		router.RegisterFiltered("replacement", []string{"managed/#"}, func(*pahov5.Publish, func() error) {
			delivered <- struct{}{}
		})
		<-delivered
		assertRouterReservations(t, router, 0)
	})

	t.Run("shutdown unblocks waiter", func(t *testing.T) {
		router := newRouter(nil, nil, withDispatchCapacity(1))
		first := &pahov5.Publish{Topic: "pending/first", QoS: 1}
		require.True(t, router.reserveQueueSlot(first, first.QoS))
		waiterDone := make(chan bool, 1)
		go func() {
			second := &pahov5.Publish{Topic: "pending/second", QoS: 2}
			waiterDone <- router.reserveQueueSlot(second, second.QoS)
		}()

		router.shutdown()
		assert.False(t, <-waiterDone)
		assertRouterReservations(t, router, 0)
	})

	t.Run("concurrent cleanup", func(t *testing.T) {
		router := newRouter(nil, nil, withDispatchCapacity(1))
		pub := &pahov5.Publish{Topic: "pending/race", QoS: 1}
		require.True(t, router.reserveQueueSlot(pub, pub.QoS))

		var releases sync.WaitGroup
		for range 16 {
			releases.Add(1)
			go func() {
				defer releases.Done()
				router.releaseQueueReservation(pub)
			}()
		}
		releases.Wait()
		assertRouterReservations(t, router, 0)
	})
}

func assertRouterReservations(t *testing.T, router *router, want int) {
	t.Helper()
	router.mu.RLock()
	defer router.mu.RUnlock()
	assert.Equal(t, want, router.queueReserved)
	assert.Len(t, router.queueReservations, want)
}

func TestConfigIngressMemory_ExplicitUnsafePacketSizeRejectsWithoutClamp(t *testing.T) {
	cfg := Config{Session: SessionOptions{
		MaxPayloadBytes:          math.MaxUint32,
		ReceiveMaximum:           1,
		IngressMemoryBudgetBytes: math.MaxUint64,
	}}

	err := cfg.ValidateIngressMemory(0)
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func TestConfigIngressMemory_DirectMapIntegerBoundaries(t *testing.T) {
	twoTo64 := math.Ldexp(1, 64)
	nextBelowTwoTo64 := math.Nextafter(twoTo64, 0)
	tests := []struct {
		name      string
		key       string
		value     any
		wantErr   bool
		wantValue uint64
	}{
		{
			name: "receive maximum exact max",
			key:  "receive_maximum", value: uint64(math.MaxUint16),
			wantValue: math.MaxUint16,
		},
		{
			name: "receive maximum max plus one",
			key:  "receive_maximum", value: uint64(math.MaxUint16) + 1,
			wantErr: true,
		},
		{
			name: "payload exact uint32 max",
			key:  "max_payload_bytes", value: uint64(math.MaxUint32),
			wantValue: math.MaxUint32,
		},
		{
			name: "payload uint32 max plus one",
			key:  "max_payload_bytes", value: uint64(math.MaxUint32) + 1,
			wantErr: true,
		},
		{
			name: "ingress budget exact uint64 max",
			key:  "ingress_memory_budget_bytes", value: uint64(math.MaxUint64),
			wantValue: math.MaxUint64,
		},
		{
			name: "float nextafter below two to 64 accepted for uint64",
			key:  "ingress_memory_budget_bytes", value: nextBelowTwoTo64,
			wantValue: uint64(nextBelowTwoTo64),
		},
		{
			name: "float nextafter below two to 64 rejected for uint16",
			key:  "receive_maximum", value: nextBelowTwoTo64,
			wantErr: true,
		},
		{
			name: "float exactly two to 64 rejected",
			key:  "ingress_memory_budget_bytes", value: twoTo64,
			wantErr: true,
		},
		{
			name: "negative int64 rejected",
			key:  "ingress_memory_budget_bytes", value: int64(-1),
			wantErr: true,
		},
		{
			name: "nan rejected",
			key:  "ingress_memory_budget_bytes", value: math.NaN(),
			wantErr: true,
		},
		{
			name: "positive infinity rejected",
			key:  "ingress_memory_budget_bytes", value: math.Inf(1),
			wantErr: true,
		},
		{
			name: "negative infinity rejected",
			key:  "ingress_memory_budget_bytes", value: math.Inf(-1),
			wantErr: true,
		},
		{
			name: "fraction rejected",
			key:  "ingress_memory_budget_bytes", value: 1.5,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, err := SessionOptionsFromMap(map[string]any{test.key: test.value})
			if test.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, shared.ErrInvalidConfig)
				return
			}
			require.NoError(t, err)
			var value uint64
			switch test.key {
			case "receive_maximum":
				value = uint64(options.ReceiveMaximum)
			case "max_payload_bytes":
				value = uint64(options.MaxPayloadBytes)
			case "ingress_memory_budget_bytes":
				value = options.IngressMemoryBudgetBytes
			}
			assert.Equal(t, test.wantValue, value)
		})
	}
}
