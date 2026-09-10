package paho

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// Baselines for the work added on two paths that run per activation and per
// message: classifying broker acknowledgement reason codes, and validating
// subscriptions before they reach the broker.

// BenchmarkClassifySubackReasons measures the per-SUBACK cost of walking every
// reason code, at the sizes a real session subscribes in. The rejected variant
// is the path a denied filter takes, which is now classified rather than
// discarded.
func BenchmarkClassifySubackReasons(b *testing.B) {
	for _, size := range []int{1, 8, 64} {
		toSub := make([]subscribeSpec, size)
		granted := make([]byte, size)
		rejected := make([]byte, size)
		for i := range toSub {
			toSub[i] = subscribeSpec{Topic: fmt.Sprintf("sensors/%d/temp", i), QoS: 1}
			granted[i] = 1
			rejected[i] = 1
		}
		rejected[size-1] = 0x87

		b.Run(fmt.Sprintf("granted/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _, _ = classifySubackReasons(toSub, granted)
			}
		})
		b.Run(fmt.Sprintf("rejected/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _, _ = classifySubackReasons(toSub, rejected)
			}
		})
	}
}

// BenchmarkValidateMQTTSubscription measures the filter and QoS check now run
// once per subscription at the factory seam and again in reconcile.
func BenchmarkValidateMQTTSubscription(b *testing.B) {
	cases := map[string]string{
		"concrete": "devices/factory-a/line-3/sensor-17/temperature",
		"wildcard": "devices/+/line-3/#",
		"shared":   "$share/workers/devices/+/line-3/#",
	}
	for name, filter := range cases {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = ValidateMQTTSubscription(filter, 1)
			}
		})
	}
}

// BenchmarkReconcile_SubscribePlan measures a whole reconcile of a fresh plan
// against a stub connection — validation, delta computation, SUBACK
// classification and state commit — at the sizes a session activates with.
func BenchmarkReconcile_SubscribePlan(b *testing.B) {
	for _, size := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("filters/%d", size), func(b *testing.B) {
			subs := make([]connectivity.SubscriptionPlan, size)
			for i := range subs {
				subs[i] = connectivity.SubscriptionPlan{Topic: fmt.Sprintf("sensors/%d/temp", i), QoS: 1}
			}
			plan := connectivity.SessionPlan{Subscriptions: subs}
			conn := &ackAndErrorConn{}
			ctx := context.Background()

			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				s := NewSession(SessionOptions{
					BrokerURLs: []string{"tcp://192.0.2.1:1883"},
					ClientID:   "bench-reconcile",
				}, connectivity.SessionEphemeral, nil)
				s.cm = conn
				b.StartTimer()

				if err := s.reconcile(ctx, conn, plan, nil, 0); err != nil {
					b.Fatalf("reconcile: %v", err)
				}
			}
		})
	}
}

// BenchmarkPublishAckClassification measures the sender's rejected-publish
// path: the reason code is mapped before the SDK error, which is the branch a
// broker denial now takes on every attempt.
func BenchmarkPublishAckClassification(b *testing.B) {
	sender := &Sender{opts: SenderOptions{QoS: 1}, metrics: &noopTestExporter{}}
	sdkErr := errors.New("error publishing: Not authorized")

	b.ReportAllocs()
	for b.Loop() {
		resp := publishResult{ReasonCode: 0x87, Acknowledged: true}
		if resp.Acknowledged {
			if berr := sender.publishReasonError(resp.ReasonCode, "denied/topic"); berr != nil {
				continue
			}
		}
		_ = MapError(sdkErr)
	}
}
