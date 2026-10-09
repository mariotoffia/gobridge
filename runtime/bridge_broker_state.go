package runtime

import (
	"context"
	"maps"
	"slices"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// Ending broker state (ADR 0024). A reload that retires a session holding a
// broker state key the next configuration no longer has, or a full swap that
// stops the runtime running it, asks the components that hold that state to
// end it when they close: the session (MQTT) and the receivers reading through
// it (AMQP 1.0). Receivers are asked before the unit's runs are cancelled,
// because the route runner closes them inside the run; sessions are asked just
// before their manager closes them, after every run stopped. Nothing is asked
// when the drain before the cancel did not settle every delivery, since ending
// the state would delete a received but unsettled one. A lease-managed
// session's lease is released only after its Close returned. A receiver whose
// session no manager runs is never asked, since nothing tells whether this
// instance may end that session's state.

// endingSessions returns the ids of ids that keep accepts, as a set; a nil keep
// accepts every id.
func endingSessions(ids []string, keep func(string) bool) map[string]bool {
	ending := make(map[string]bool, len(ids))
	for _, id := range ids {
		if keep == nil || keep(id) {
			ending[id] = true
		}
	}
	return ending
}

// keepUnsettledBrokerState is called when the drain before the cancel did not
// settle every delivery: the broker holds the only copy of one still
// unsettled, which ending the state would delete. For each session in ending
// that a manager in managers runs and this instance may end, it logs a Warn and
// counts shared.MetricBrokerStateEndFailures. It returns nil, so nothing ends.
func (rt *Runtime) keepUnsettledBrokerState(ending map[string]bool, managers map[string]*session.Manager) map[string]bool {
	for _, sid := range slices.Sorted(maps.Keys(ending)) {
		mgr, managed := managers[sid]
		if !managed {
			continue
		}
		if _, may := mgr.MayEndBrokerState(); !may {
			continue
		}
		if rt.metrics != nil {
			rt.metrics.Counter(shared.MetricBrokerStateEndFailures, 1, shared.Tag{Key: shared.TagKeySessionID, Value: sid})
		}
		if rt.logger != nil {
			rt.logger.Warn("broker state kept: in-flight deliveries did not settle before the cancel, "+
				"and ending the state would delete them; the broker keeps it until it expires or an operator deletes it",
				"session_id", sid)
		}
	}
	return nil
}

// askReceiversToEndBrokerState asks every receiver of entries that reads
// through a session in ending that a manager in managers runs, and implements
// ports.BrokerStateEnder, to end its broker state when its route closes it, if
// this instance may end it (session.Manager.MayEndBrokerState). A receiver is
// matched to its session by its route's SourceSessionID, which the bridge
// builder sets for every route that reads through a session; a route without
// one is not asked and keeps its state. The ask carries the earlier of the
// local deadline of that session's lease and ctx's deadline less
// storeCloseGraceMargin: the route runner closes the receiver inside the run
// the teardown waits for, under a close budget of its own that may outlast ctx,
// so an ending the broker never acknowledges gives up while that run can still
// finish in time. managers are the session managers by session id.
func askReceiversToEndBrokerState(ctx context.Context, entries []*routeEntry, ending map[string]bool, managers map[string]*session.Manager) {
	teardownEnd, bounded := ctx.Deadline()
	if bounded {
		teardownEnd = teardownEnd.Add(-storeCloseGraceMargin)
	}
	for _, entry := range entries {
		sid := entry.config.SourceSessionID
		if !ending[sid] {
			continue
		}
		mgr := managers[sid]
		if mgr == nil {
			continue
		}
		before, ok := mgr.MayEndBrokerState()
		if !ok {
			continue
		}
		if bounded && (before.IsZero() || teardownEnd.Before(before)) {
			before = teardownEnd
		}
		if ender, isEnder := entry.receiver.(ports.BrokerStateEnder); isEnder {
			ender.EndBrokerStateOnClose(before)
		}
	}
}

// closeManager closes mgr, ending its session's broker state when end is set.
func closeManager(ctx context.Context, mgr *session.Manager, end bool) error {
	if end {
		return mgr.CloseEndingBrokerState(ctx)
	}
	return mgr.Close(ctx)
}
