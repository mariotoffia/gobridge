package runtime

import (
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
)

// How long one message may hold its source before it is settled, and what puts a
// ceiling on that hold. A fixed visibility window redelivers when it runs out; a
// source session that recycles to recover stranded settlements gives up when the
// deliveries it already accepted do not settle in time. Both ceilings are known
// before the first message, so the checks here are static.

// validateTimeouts checks that SendTimeout does not exceed half the
// source visibility timeout. When send takes longer than the visibility
// window, the source transport redelivers the message while send is
// still in progress, causing duplicates.
//
// The check is skipped when the source auto-extends the window
// (SourceAutoExtend): a background renewal keeps the message invisible
// for the duration of processing, so a deliberately short window is safe
// and must not be rejected. It only guards routes running with a fixed,
// non-renewed window.
func validateTimeouts(ve *ValidationError, prefix string, entry *routeEntry, hasDLQStore bool) {
	policy := entry.config.Policy.WithDefaults()
	vis := entry.config.SourceVisibilityTimeout
	if entry.config.SourceAutoExtend || vis <= 0 {
		// Auto-extend renews the window in the background, and a zero/unknown
		// window is not checkable; either way the fixed-window bound does not apply.
		return
	}

	if policy.SendTimeout >= vis/2 {
		ve.add(prefix + fmt.Sprintf(
			"SendTimeout (%s) >= VisibilityTimeout/2 (%s); "+
				"source may redeliver before send completes",
			policy.SendTimeout, vis/2))
	}

	// Total worst-case time the message can hold the source before it is settled,
	// checked against the fixed visibility window. Per-processor budgets are
	// own-time-only and disarm during next() (route/chain.go), so N compliant
	// processors can legally consume N×ProcessorTimeout before the send even
	// starts; add the in-process send-retry budget, the send budget and the
	// bounded DLQ-write budget the failure path may spend. When this total exceeds
	// the window the source redelivers mid-pipeline and the message is processed
	// concurrently — the very duplicate this validator exists to prevent — even
	// though every individual timeout passes its own check.
	//
	// dlqWriteBudget is the DLQ-write time the failure path may spend before it
	// settles the source (see the package const). It is counted into the worst
	// case only when a DLQ write is actually reachable: a DLQ store exists AND the
	// terminal policy is not drop. A drop-policy route, or any route in a
	// deployment with no DLQ store, settles the source in-memory on failure, so
	// counting the budget there would over-reject a safe config at startup.
	dlqBudget := time.Duration(0)
	if hasDLQStore && policy.OnPermanentFailure != routing.FailureDrop {
		dlqBudget = dlqWriteBudget
	}
	nProc := len(entry.config.Processors)
	retry := sendRetryBudgetFor(policy)
	total := time.Duration(nProc)*policy.ProcessorTimeout + retry + policy.SendTimeout + dlqBudget
	if total > vis {
		fix := ""
		if retry > 0 {
			fix = " (lower send_retry_budget, set it to 0s to turn in-process send retry off, " +
				"or auto-extend the source window)"
		}
		ve.add(prefix + fmt.Sprintf(
			"worst-case pipeline time (%s = %d processors × ProcessorTimeout %s + SendRetryBudget %s + "+
				"SendTimeout %s + DLQ budget %s) exceeds source VisibilityTimeout (%s); "+
				"source may redeliver mid-pipeline causing duplicate processing%s",
			total, nProc, policy.ProcessorTimeout, retry, policy.SendTimeout, dlqBudget, vis, fix))
	}
}

// dlqWriteBudget is the bounded wall-clock time the inline failure path may spend
// writing a poisoned/permanently-failed message to the DLQ before it settles the
// source. It mirrors the DLQ router wiring in bridge_start.go (WriteTimeout 5s ×
// WriteMaxAttempts 2 plus the router's 500ms default backoff between attempts =
// 10.5s). Kept a local constant because the runtime does not model it as a shared
// value; TestDLQWriteBudget_MatchesRouterWiring pins it to that wiring so either
// side drifting is caught.
const dlqWriteBudget = 10500 * time.Millisecond

// sendRetryBudgetFor is the in-process send-retry time one delivery may add to
// its hold on the source. Only a direct_hold route retries a send while the
// source message is still unsettled, and only while the budget is enabled —
// every budget at or below zero means the route sends once, as it always did.
func sendRetryBudgetFor(policy routing.RoutePolicy) time.Duration {
	if policy.DeliveryMode != routing.DeliveryDirectHold || policy.SendRetryBudget <= 0 {
		return 0
	}
	return policy.SendRetryBudget
}

// validateSendRetryBudget checks the in-process send-retry budget itself.
//
// A negative value other than the opt-out sentinel is rejected: WithDefaults
// fills only a zero budget, so it would reach the runtime, where every budget at
// or below zero silently reads as "no in-process retry" — not what an operator
// who typed one meant.
//
// The second check is the source's settlement-recovery wait. A session that
// recycles its broker connection to recover stranded settlements first waits a
// bounded time for the deliveries it already accepted to settle; a held retry
// still running when that wait runs out fails the recovery attempt and
// terminalizes the session. The retry loop starts its last send before the
// budget ends and that send may then run its full SendTimeout, so budget +
// SendTimeout is the hold the wait has to cover.
func validateSendRetryBudget(ve *ValidationError, prefix string, entry *routeEntry, policy routing.RoutePolicy) {
	if policy.SendRetryBudget < 0 && policy.SendRetryBudget != routing.SendRetryBudgetDisabled {
		ve.add(prefix + fmt.Sprintf(
			"SendRetryBudget (%s) must not be negative; use routing.SendRetryBudgetDisabled "+
				"to turn in-process send retry off",
			policy.SendRetryBudget))
	}

	retry := sendRetryBudgetFor(policy)
	wait := entry.config.SourceSettlementRecoveryWait
	if retry <= 0 || wait <= 0 {
		return
	}
	if retry+policy.SendTimeout > wait {
		ve.add(prefix + fmt.Sprintf(
			"send_retry_budget %s + send_timeout %s exceeds the source's settlement-recovery wait %s; "+
				"a held retry would outlive the MQTT connection recycle and fail it "+
				"(lower send_retry_budget or send_timeout, or raise the session's connect/reconcile timeouts)",
			retry, policy.SendTimeout, wait))
	}
}
