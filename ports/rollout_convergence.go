package ports

// Whether a member's post-swap readiness actually PROVES anything.
//
// The readiness fold deliberately skips a session that defers its connect until
// this instance wins a lease and has not won it (ReadinessLevelFromDeepHealth).
// That is right for a readiness probe: a warm standby is doing exactly what it
// should, and capping it below ready would shed traffic from a healthy instance.
//
// It is not enough for the confirm window. There, the question is not "is this
// member ready" but "has this member DEMONSTRATED that the config it just swapped
// to can serve" — and a member whose every session was skipped has demonstrated
// nothing. On a lease-based cohort that is every member for the first seconds
// after a swap, including the one that is about to take the lease: it has not
// re-acquired yet, so its broker session is skipped, and it folds to "ready" over
// an empty set. Left at that, all three members record convergence before any of
// them has spoken to the broker, the coordinator confirms, and a config none of
// them can run becomes permanent — the exact outcome the window exists to
// prevent.
//
// So the confirm window asks two questions instead of one.

// RolloutConvergence reports whether dh describes a member that has reached its
// post-swap readiness (ready), and whether that answer rests on anything
// observed (provable).
//
// provable is false only when the member has sessions and EVERY one of them was
// skipped as a dormant deferred-connect session. A member with no stateful
// sessions at all is provable: there is nothing that could later contradict its
// readiness. A member holding the lease is provable: its session was folded in.
//
// A member that is ready-but-not-provable has not earned a convergence record on
// its own. What it may do is follow one: once some OTHER member has recorded
// convergence for the same generation, the cohort has the demonstration it
// needed, and a genuine standby — which can never produce one itself — is free to
// agree. If no member ever produces one, nobody converges and the window expires,
// which is the correct outcome for a change nothing could verify.
func RolloutConvergence(dh DeepHealth) (ready, provable bool) {
	if ReadinessLevelFromDeepHealth(dh) < LevelSubscribed {
		return false, false
	}
	if len(dh.Sessions) == 0 {
		return true, true
	}
	for _, session := range dh.Sessions {
		if session.ConnectAfterLease && !session.HasLease {
			continue // the same session the readiness fold skipped
		}
		return true, true
	}
	return true, false
}
