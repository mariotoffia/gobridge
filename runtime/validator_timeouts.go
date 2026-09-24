package runtime

import (
	"fmt"
	"math"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/runtime/route"
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
	// starts; add the in-process send-retry budget, the longest one send can hold
	// the source (sendHoldFor) and the bounded DLQ-write budget the failure path
	// may spend. When this total exceeds the window the source redelivers
	// mid-pipeline and the message is processed concurrently — the very duplicate
	// this validator exists to prevent — even though every individual timeout
	// passes its own check.
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
	sendHold := sendHoldFor(policy)
	procHold, procClamped := saturatingProduct(nProc, policy.ProcessorTimeout)
	total, sumClamped := saturatingSum(procHold, retry, sendHold, dlqBudget)
	clamped := procClamped || sumClamped
	if clamped || total > vis {
		shown := total.String()
		if clamped {
			shown = "over " + shown
		}
		fix := ""
		if retry > 0 {
			fix = " (lower send_retry_budget, set it to 0s to turn in-process send retry off, " +
				"or auto-extend the source window)"
		}
		sendTerm := policy.SendTimeout.String()
		if grace := sendHold - policy.SendTimeout; grace > 0 {
			sendTerm += fmt.Sprintf(" (+%s wedge grace)", grace)
		}
		ve.add(prefix + fmt.Sprintf(
			"worst-case pipeline time (%s = %d processors × ProcessorTimeout %s + SendRetryBudget %s + "+
				"SendTimeout %s + DLQ budget %s) exceeds source VisibilityTimeout (%s); "+
				"source may redeliver mid-pipeline causing duplicate processing%s",
			shown, nProc, policy.ProcessorTimeout, retry, sendTerm, dlqBudget, vis, fix))
	}
}

// sendHoldFor is the longest one physical send can hold the SOURCE message. A
// direct_hold route sends while the source is still unsettled, so a sender that
// ignores its context holds it until route.SendWedgeCeiling — the bound dispatch
// enforces, read from the same function so the two cannot drift. Every other
// delivery mode settles its source before it sends (shared_outbox acks once the
// outbox record is persisted), so the wedge grace is no part of its hold and
// its term stays SendTimeout, exactly as before in-process send retry existed.
func sendHoldFor(policy routing.RoutePolicy) time.Duration {
	if policy.DeliveryMode != routing.DeliveryDirectHold {
		return policy.SendTimeout
	}
	return route.SendWedgeCeiling(policy.SendTimeout)
}

// maxDuration is the ceiling the two helpers below clamp at.
const maxDuration = time.Duration(math.MaxInt64)

// saturatingSum and saturatingProduct build the worst-case hold above. Every
// term is a duration parsed from configuration, so any of them may legitimately
// be near maxDuration; added with plain arithmetic the total wraps NEGATIVE,
// and a negative total is under every visibility window — so the route with the
// longest possible hold would be the one this check waves through. Clamping
// keeps an absurd term absurd.
//
// Both helpers also report whether they clamped, and the caller rejects on a
// clamp whatever the window. The clamped value alone is not enough: a source
// may report a window of exactly maxDuration, a clamped total compares EQUAL to
// it, and `total > vis` would admit a hold that is really longer. A clamp means
// the true total is past maxDuration, so it is past every window a source can
// have. The runtime has no shared saturating duration helper to reuse (the
// failover-budget sum lives in the composition root and the MQTT one is
// adapter-local; neither may be imported here), so these stay unexported in
// this package.
//
// Non-positive terms are skipped rather than added: every term is a
// validated-or-defaulted budget, and a negative one must not shrink the worst
// case into passing.
func saturatingSum(parts ...time.Duration) (total time.Duration, clamped bool) {
	for _, part := range parts {
		if part <= 0 {
			continue
		}
		if total > maxDuration-part {
			return maxDuration, true
		}
		total += part
	}
	return total, false
}

// saturatingProduct is n copies of d with the same clamp.
func saturatingProduct(n int, d time.Duration) (product time.Duration, clamped bool) {
	if n <= 0 || d <= 0 {
		return 0, false
	}
	if int64(n) > int64(maxDuration)/int64(d) {
		return maxDuration, true
	}
	return time.Duration(n) * d, false
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
// budget ends and that send may then hold the delivery until its wedge ceiling
// (sendHoldFor — the same per-send bound the fixed-window sum above counts;
// only a direct_hold route with an enabled budget gets this far), so budget +
// that ceiling is the hold the wait has to cover. A source that reports a
// negative wait is rejected rather than skipped: zero is the capability's no-op,
// so a negative value is a broken transport, not a source without a recycle.
func validateSendRetryBudget(ve *ValidationError, prefix string, entry *routeEntry, policy routing.RoutePolicy) {
	if policy.SendRetryBudget < 0 && policy.SendRetryBudget != routing.SendRetryBudgetDisabled {
		ve.add(prefix + fmt.Sprintf(
			"SendRetryBudget (%s) must not be negative; use routing.SendRetryBudgetDisabled "+
				"to turn in-process send retry off",
			policy.SendRetryBudget))
	}

	retry := sendRetryBudgetFor(policy)
	wait := entry.config.SourceSettlementRecoveryWait
	if retry <= 0 {
		// No in-process retry means no held delivery for a recycle to wait on,
		// so this route has no hold for the source's wait to cover.
		return
	}
	if wait < 0 {
		// ZERO is the capability's documented no-op — "this session never
		// recycles to recover stranded settlements" — and skipping the check on
		// it is right. A NEGATIVE wait is not a second way of saying that: it is
		// a SettlementRecoveryTimingConfig that is broken, and reading it as "no
		// check" would silently disarm this gate for the one route that most
		// needs it. Fail closed and name the reported value, so an operator sees
		// a misbehaving transport instead of losing a safety check.
		ve.add(prefix + fmt.Sprintf(
			"source settlement-recovery wait (%s) is negative; a transport reports zero when its "+
				"session never recycles to recover stranded settlements, so this is a broken "+
				"SettlementRecoveryTimingConfig and the held-retry check cannot run",
			wait))
		return
	}
	if wait == 0 {
		return
	}
	// Compare by SUBTRACTION, never by adding the two holds together: both come
	// from parsed configuration and either may be near the largest duration
	// there is, and a wrapped sum is under every wait. Both terms here are
	// positive — the guards above return on every non-positive wait, WithDefaults
	// fills a non-positive send timeout, and the ceiling saturates rather than
	// wrapping — so wait - sendHold cannot underflow, and a send that alone
	// outlives the wait leaves a negative remainder that every enabled budget
	// exceeds.
	sendHold := sendHoldFor(policy)
	if retry > wait-sendHold {
		ve.add(prefix + fmt.Sprintf(
			"send_retry_budget %s + send_timeout %s (+%s wedge grace) exceeds the source's "+
				"settlement-recovery wait %s; a held retry would outlive the wait and fail the source's recycle "+
				"(lower send_retry_budget or send_timeout, or raise the source session's "+
				"settlement-recovery wait through its transport's own timeouts)",
			retry, policy.SendTimeout, sendHold-policy.SendTimeout, wait))
	}
}
