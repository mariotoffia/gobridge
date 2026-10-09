package runtime

import (
	"context"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// Ending broker state (ADR 0024). A reload that retires a session holding a
// broker state key the next configuration no longer has, or a full swap that
// stops the runtime running it, asks the components that hold that state to
// end it when they close: the session (MQTT) and the receivers reading through
// it (AMQP 1.0). Receivers are asked before the unit's runs are cancelled,
// because the route runner closes them inside the run; sessions are asked just
// before their manager closes them, after every run stopped. A lease-managed
// session's lease is released only after its Close returned. Every configured
// session has a manager; a session none runs carries the empty id, which no
// reload names, so it is never asked.

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

// mayEndBrokerState reports whether this instance may end the broker state of
// a session mgr runs. A session no manager runs holds no lease.
func mayEndBrokerState(mgr *session.Manager) bool {
	return mgr == nil || mgr.MayEndBrokerState()
}

// askReceiversToEndBrokerState asks every receiver of entries that reads
// through a session in ending, and implements ports.BrokerStateEnder, to end
// its broker state when its route closes it. managers are the session managers
// by session id.
func askReceiversToEndBrokerState(entries []*routeEntry, ending map[string]bool, managers map[string]*session.Manager) {
	for _, entry := range entries {
		sid := entry.config.SourceSessionID
		if !ending[sid] || !mayEndBrokerState(managers[sid]) {
			continue
		}
		if ender, ok := entry.receiver.(ports.BrokerStateEnder); ok {
			ender.EndBrokerStateOnClose()
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
