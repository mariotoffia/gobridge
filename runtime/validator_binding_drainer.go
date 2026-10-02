package runtime

import (
	"fmt"

	"github.com/mariotoffia/gobridge/domain/routing"
)

// validateBindingDrainerLeases refuses a shared_outbox binding whose session
// gets its own outbox drainer from a manager that is not lease-managed. The
// drainer is gated on that manager's lease, and only an exclusive manager in a
// runtime with a lease store ever holds one; for any other the drainer skips
// every cycle and the records persisted for the binding never drain while their
// sources are acknowledged.
//
// It replays wireRouteEntriesLocked's order, because the manager the drainer
// gets is not necessarily built from the session sender: a session has one
// manager, built from the first registration wiring reaches (a route's own
// session block, else the session sender), and a partition has one drainer,
// built by the first route that reaches it. A route's drainer on its own
// session block is checked by validateSharedOutbox.
func validateBindingDrainerLeases(ve *ValidationError, entries []*routeEntry, senders map[string]*sessionSenderEntry, hasLeaseStore bool) {
	type manager struct {
		exclusive bool
		routeID   string // route whose session block built it; "" for the session sender
	}
	managers := make(map[string]manager)
	drained := make(map[string]bool)
	for _, entry := range entries {
		sharedOutbox := entry.config.Policy.DeliveryMode == routing.DeliverySharedOutbox
		if entry.session != nil && entry.sessCfg != nil {
			sid := entry.sessCfg.SessionID
			if _, ok := managers[sid]; !ok {
				managers[sid] = manager{exclusive: entry.sessCfg.Exclusive, routeID: entry.config.ID}
			}
			if sharedOutbox {
				drained[sid] = true
			}
		}
		if !sharedOutbox {
			continue
		}
		for _, b := range entry.config.Bindings {
			sid := b.SessionID
			if sid == "" && entry.sessCfg != nil {
				sid = entry.sessCfg.SessionID // the binding inherits the route's session block
			}
			if sid == "" || drained[sid] {
				continue
			}
			sse, ok := senders[sid]
			if !ok {
				continue
			}
			drained[sid] = true
			mgr, ok := managers[sid]
			if !ok {
				mgr = manager{exclusive: sse.config.Exclusive}
				managers[sid] = mgr
			}
			if mgr.exclusive && hasLeaseStore {
				continue
			}
			prefix := fmt.Sprintf("route %q: ", entry.config.ID)
			switch {
			case !mgr.exclusive && mgr.routeID != "":
				ve.add(prefix + fmt.Sprintf(
					"shared_outbox invalid: binding session %q is managed under route %q's non-exclusive "+
						"session config; a non-exclusive session never acquires a lease, so its outbox "+
						"drainer skips every cycle and persisted records never drain (make the session "+
						"exclusive)", sid, mgr.routeID))
			case !mgr.exclusive:
				ve.add(prefix + fmt.Sprintf(
					"shared_outbox invalid: binding session %q is non-exclusive; a non-exclusive "+
						"session never acquires a lease, so its outbox drainer skips every cycle and "+
						"persisted records never drain (make the session exclusive)", sid))
			default:
				ve.add(prefix + fmt.Sprintf(
					"shared_outbox invalid: no LeaseStore configured for binding session %q; its "+
						"outbox drainer waits for a lease that nothing grants, so persisted records "+
						"never drain (a LeaseStore is required)", sid))
			}
		}
	}
}
