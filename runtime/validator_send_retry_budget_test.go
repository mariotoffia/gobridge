package runtime_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// An in-process send retry keeps the source message unsettled while it runs, so
// the budget is time the SOURCE spends waiting. Two sources put a ceiling on
// that wait: a fixed visibility window redelivers when it runs out, and an MQTT
// session that recycles to recover stranded settlements gives up when the
// deliveries it already accepted do not settle in time. Both ceilings are known
// before the first message, so a budget that cannot fit under them is a config
// error, not a production incident.

// validationMessages runs the side-effect-free route validation and returns the
// collected messages (none when the configuration is valid).
func validationMessages(t *testing.T, rt *runtime.Runtime) []string {
	t.Helper()
	err := rt.ValidateRoutes()
	if err == nil {
		return nil
	}
	var ve *runtime.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("ValidateRoutes returned %T, want *runtime.ValidationError: %v", err, err)
	}
	return ve.Errors()
}

// containsMessage reports whether any validation message contains want.
func containsMessage(messages []string, want string) bool {
	for _, m := range messages {
		if strings.Contains(m, want) {
			return true
		}
	}
	return false
}

// TestValidator_FixedWindowCountsTheSendRetryBudget pins the budget as part of
// the worst-case time one message holds a fixed source window: the retry loop
// runs while the message is unsettled, so a window that fits the processors, the
// send and the dead-letter write can still be too small once the route retries
// in process. A disabled budget holds the source exactly as before, and a
// shared_outbox route never retries in process at all — its record is persisted
// and the source is settled.
func TestValidator_FixedWindowCountsTheSendRetryBudget(t *testing.T) {
	cases := []struct {
		name         string
		deliveryMode routing.DeliveryMode
		budget       time.Duration
		window       time.Duration
		wantRejected bool
	}{
		// 60s budget + 30s send + 10.5s DLQ budget = 100.5s.
		{name: "default budget fits the window", deliveryMode: routing.DeliveryDirectHold,
			window: 120 * time.Second},
		{name: "default budget outlives the window", deliveryMode: routing.DeliveryDirectHold,
			window: 90 * time.Second, wantRejected: true},
		// 30s send + 10.5s DLQ budget = 40.5s once in-process retry is off.
		{name: "disabled budget is not counted", deliveryMode: routing.DeliveryDirectHold,
			budget: routing.SendRetryBudgetDisabled, window: 90 * time.Second},
		{name: "shared_outbox never retries in process", deliveryMode: routing.DeliverySharedOutbox,
			window: 90 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := runtime.New(
				runtime.WithInstanceID("test-bridge"),
				runtime.WithDLQStore(NewFakeDLQStore()),
			)
			cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
			cfg.Policy.DeliveryMode = tc.deliveryMode
			cfg.Policy.OnPermanentFailure = routing.FailureDLQ
			cfg.Policy.SendTimeout = 30 * time.Second
			cfg.Policy.SendRetryBudget = tc.budget
			cfg.SourceVisibilityTimeout = tc.window
			cfg.SourceAutoExtend = false

			if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
				t.Fatal(err)
			}

			messages := validationMessages(t, rt)
			rejected := containsMessage(messages, "worst-case pipeline time")
			if rejected != tc.wantRejected {
				t.Fatalf("rejected for pipeline time = %v, want %v: %v", rejected, tc.wantRejected, messages)
			}
			if tc.wantRejected && !containsMessage(messages, "SendRetryBudget 1m0s") {
				t.Fatalf("the rejection must name the budget it counted: %v", messages)
			}
			if tc.wantRejected && !containsMessage(messages, "send_retry_budget") {
				t.Fatalf("the rejection must name the knob that lowers it: %v", messages)
			}
		})
	}
}

// TestValidator_SendRetryBudgetAgainstSettlementRecoveryWait pins the second
// ceiling. An MQTT session that recycles to recover stranded settlements waits a
// bounded time for the deliveries the runtime already accepted to settle; a held
// retry that is still running when that wait runs out fails the recycle and
// terminalizes the session. The last send starts before the budget ends and may
// then run its full SendTimeout, so budget + send timeout is what has to fit.
func TestValidator_SendRetryBudgetAgainstSettlementRecoveryWait(t *testing.T) {
	cases := []struct {
		name         string
		deliveryMode routing.DeliveryMode
		budget       time.Duration
		sendTimeout  time.Duration
		wait         time.Duration
		wantRejected bool
		wantMessage  string
	}{
		{name: "default budget fits the recycle wait", deliveryMode: routing.DeliveryDirectHold,
			sendTimeout: 30 * time.Second, wait: 240 * time.Second},
		{name: "budget and send timeout exactly fill the recycle wait", deliveryMode: routing.DeliveryDirectHold,
			budget: 210 * time.Second, sendTimeout: 30 * time.Second, wait: 240 * time.Second},
		{name: "budget and send timeout outlive the recycle wait", deliveryMode: routing.DeliveryDirectHold,
			budget: 220 * time.Second, sendTimeout: 30 * time.Second, wait: 240 * time.Second, wantRejected: true,
			wantMessage: "send_retry_budget 3m40s + send_timeout 30s"},
		// Zero is the capability's no-op: the session never recycles for
		// settlement recovery, so there is nothing to fit inside.
		{name: "a source that never recycles is not checked", deliveryMode: routing.DeliveryDirectHold,
			budget: 220 * time.Second, sendTimeout: 30 * time.Second},
		// A NEGATIVE wait is not a second way of saying zero — it is a broken
		// SettlementRecoveryTimingConfig. Reading it as "no check" would turn
		// the gate off for the route that most needs it, so it fails closed and
		// names the value the transport reported.
		{name: "a negative reported wait is a broken transport, not a no-op", deliveryMode: routing.DeliveryDirectHold,
			budget: 220 * time.Second, sendTimeout: 30 * time.Second, wait: -5 * time.Second, wantRejected: true,
			wantMessage: "source settlement-recovery wait (-5s) is negative"},
		// A disabled budget holds the delivery for one send, exactly as a route
		// did before in-process retry existed, so the recycle wait is the
		// operator's own pre-existing SendTimeout choice to make.
		{name: "a disabled budget is never checked", deliveryMode: routing.DeliveryDirectHold,
			budget: routing.SendRetryBudgetDisabled, sendTimeout: 250 * time.Second, wait: 240 * time.Second},
		{name: "shared_outbox settles the source before it sends", deliveryMode: routing.DeliverySharedOutbox,
			budget: 220 * time.Second, sendTimeout: 30 * time.Second, wait: 240 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := runtime.New(runtime.WithInstanceID("test-bridge"))
			cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
			cfg.Policy.DeliveryMode = tc.deliveryMode
			cfg.Policy.SendTimeout = tc.sendTimeout
			cfg.Policy.SendRetryBudget = tc.budget
			cfg.SourceSettlementRecoveryWait = tc.wait

			if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
				t.Fatal(err)
			}

			messages := validationMessages(t, rt)
			rejected := containsMessage(messages, "settlement-recovery wait")
			if rejected != tc.wantRejected {
				t.Fatalf("rejected against the recycle wait = %v, want %v: %v", rejected, tc.wantRejected, messages)
			}
			if tc.wantMessage != "" && !containsMessage(messages, tc.wantMessage) {
				t.Fatalf("the rejection must read %q so an operator can act on it: %v", tc.wantMessage, messages)
			}
		})
	}
}

// TestValidator_RejectsANegativeSendRetryBudget covers the value that is neither
// a duration nor the opt-out: WithDefaults fills only a zero budget, so a
// negative one reaches the runtime, where every budget at or below zero reads as
// "no in-process retry". An operator who typed -30s meant something else.
func TestValidator_RejectsANegativeSendRetryBudget(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Policy.SendRetryBudget = -30 * time.Second

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	messages := validationMessages(t, rt)
	if !containsMessage(messages, "SendRetryBudget (-30s) must not be negative") {
		t.Fatalf("a negative budget must be rejected: %v", messages)
	}
	if !containsMessage(messages, "SendRetryBudgetDisabled") {
		t.Fatalf("the rejection must name the supported opt-out: %v", messages)
	}
}

// TestValidator_AcceptsTheSendRetryBudgetOptOut is the other half: the opt-out
// sentinel is itself negative and must survive validation.
func TestValidator_AcceptsTheSendRetryBudgetOptOut(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Policy.SendRetryBudget = routing.SendRetryBudgetDisabled

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	if messages := validationMessages(t, rt); len(messages) != 0 {
		t.Fatalf("the documented opt-out must validate: %v", messages)
	}
}

// Both ceilings are checked by ADDING route durations together, and every one
// of those durations comes from parsed configuration: a `send_retry_budget` or
// a `processor_timeout` may legitimately be written near the largest duration
// there is. A sum that wraps negative is under every ceiling, so the route with
// the LONGEST possible hold on its source would be the one the check waves
// through — the exact inversion of what these rules exist to do.

// TestValidator_AbsurdDurationsStillFailTheFixedWindow pins the worst-case
// pipeline sum against terms near the maximum duration: the total saturates at
// the maximum instead of wrapping, so a fixed-window source is rejected.
func TestValidator_AbsurdDurationsStillFailTheFixedWindow(t *testing.T) {
	cases := []struct {
		name             string
		budget           time.Duration
		processorTimeout time.Duration
		processors       []ports.Processor
	}{
		{name: "send retry budget near the maximum duration",
			budget: time.Duration(math.MaxInt64)},
		// Three processors at half the maximum each: the product alone wraps,
		// before anything is added to it.
		{name: "processor timeout near the maximum duration",
			budget:           routing.SendRetryBudgetDisabled,
			processorTimeout: time.Duration(math.MaxInt64) / 2,
			processors:       []ports.Processor{&timeoutProcessor{}, &timeoutProcessor{}, &timeoutProcessor{}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := runtime.New(
				runtime.WithInstanceID("test-bridge"),
				runtime.WithDLQStore(NewFakeDLQStore()),
			)
			cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
			cfg.Policy.OnPermanentFailure = routing.FailureDLQ
			cfg.Policy.SendTimeout = 30 * time.Second
			cfg.Policy.SendRetryBudget = tc.budget
			cfg.Policy.ProcessorTimeout = tc.processorTimeout
			cfg.Processors = tc.processors
			cfg.SourceVisibilityTimeout = 120 * time.Second
			cfg.SourceAutoExtend = false

			if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
				t.Fatal(err)
			}

			messages := validationMessages(t, rt)
			if !containsMessage(messages, "worst-case pipeline time") {
				t.Fatalf("a hold that outlives every window must be rejected, not wrapped into fitting: %v", messages)
			}
		})
	}
}

// TestValidator_AbsurdDurationsStillFailTheRecoveryWait is the same guard on the
// settlement-recovery rule, plus the boundary the rewrite must not lose: a send
// timeout that alone outlives the recycle wait rejects however small the budget
// beside it is.
func TestValidator_AbsurdDurationsStillFailTheRecoveryWait(t *testing.T) {
	cases := []struct {
		name        string
		budget      time.Duration
		sendTimeout time.Duration
		wait        time.Duration
	}{
		{name: "budget near the maximum duration",
			budget: time.Duration(math.MaxInt64), sendTimeout: 30 * time.Second, wait: 240 * time.Second},
		{name: "send timeout alone outlives the wait",
			budget: time.Second, sendTimeout: 250 * time.Second, wait: 240 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := runtime.New(runtime.WithInstanceID("test-bridge"))
			cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
			cfg.Policy.SendTimeout = tc.sendTimeout
			cfg.Policy.SendRetryBudget = tc.budget
			cfg.SourceSettlementRecoveryWait = tc.wait

			if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
				t.Fatal(err)
			}

			messages := validationMessages(t, rt)
			if !containsMessage(messages, "settlement-recovery wait") {
				t.Fatalf("a held retry that outlives the recycle wait must be rejected: %v", messages)
			}
		})
	}
}
