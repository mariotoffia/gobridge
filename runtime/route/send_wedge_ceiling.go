package route

import (
	"math"
	"time"
)

// SendWedgeCeiling is the longest one physical send can hold a delivery: how
// long boundedSend lets a send run before it classifies it as GENUINELY hung
// (ctx-ignoring) and WEDGES the route. It is deliberately LARGER than
// SendTimeout — SendTimeout + min(SendTimeout, 5s) — so a COOPERATIVE sender
// that aborts AT SendTimeout via its ctx always returns through `done` and wins
// the ceiling race; only a send still parked WELL PAST SendTimeout (having
// ignored ctx the whole time) trips the wedge. Conflating the two — a bare
// SendTimeout ceiling equal to the sendCtx deadline — flaky-wedges a healthy
// route whenever a cooperative sender legitimately hits SendTimeout under load,
// turning ordinary transient slowness into a false pod restart. The per-send
// TRANSIENT timeout (retry) still happens at SendTimeout via sendCtx; only the
// wedge decision uses this larger bound. Mirrors the outbox completeBudget /
// bridge completeBudgetCeiling shape.
//
// The route validator sizes a held delivery's last send with this same bound,
// so what it accepts and what dispatch enforces cannot drift apart.
//
// A non-positive sendTimeout means no bound (SendTimeout disabled) and returns
// 0: await completion. A timeout within the grace of the largest duration
// saturates there rather than wrapping negative, which dispatch would read as
// "no bound" and the validator as a hold shorter than any window.
func SendWedgeCeiling(sendTimeout time.Duration) time.Duration {
	if sendTimeout <= 0 {
		return 0
	}
	grace := min(sendTimeout, sendWedgeCeilingMargin)
	if sendTimeout > math.MaxInt64-grace {
		return math.MaxInt64
	}
	return sendTimeout + grace
}

// sendWedgeCeilingMargin caps the extra grace past SendTimeout before a parked
// send wedges the route (see SendWedgeCeiling).
const sendWedgeCeilingMargin = 5 * time.Second
