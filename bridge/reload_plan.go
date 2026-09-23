package bridge

import (
	"slices"

	"github.com/mariotoffia/gobridge/ports"
)

// InPlaceReload is the plan of an in-place reload from running to next: the
// reload units to retire (their key is in running only) and the units to add
// (their key is in next only). Every other unit has the same content in both
// documents and keeps running untouched. There is no pairing of a changed
// unit: a change inside a unit retires the old unit and adds the new one, and
// a merge or split of units is handled the same way.
type InPlaceReload struct {
	running, next *ports.BridgeConfig
	retire, add   []reloadUnit
	// serialized: the retired units stop before the added ones are built,
	// because one of them holds an exclusive broker identity (see
	// RequiresSerializedSwap). Otherwise the added units are built while the
	// retired ones still serve.
	serialized bool
}

// PlanInPlaceReload plans replacing only the reload units that differ between
// running and next. ok is false — the caller keeps the full replacement every
// change gets otherwise — when:
//
//  1. running or next is nil;
//  2. the bridge-wide part differs: bridge settings, config_watch, stores or
//     http. A running runtime was built for those, and a part grafted onto it
//     would be built for others;
//  3. the outbox stale-claim duration a build derives differs. The outbox
//     store takes it when it is opened, and an in-place reload keeps the
//     running store open;
//  4. a retired or added unit attaches to a transport whose factory advertises
//     ports.CapHTTPEndpoint, or that transports has no factory for. The HTTP
//     transport mounts on a stdlib ServeMux, which can neither unmount a path
//     nor mount it twice, and a transport with no factory may be one that does.
//
// An empty delta — no unit retired and none added — is eligible and applies
// nothing. Nothing here validates next: the caller preflights it in full before
// any part is built, so an in-place reload is never more permissive than a full
// build. transports maps plugin kind to factory as the caller registered them.
func PlanInPlaceReload(running, next *ports.BridgeConfig, transports map[string]ports.TransportFactory) (*InPlaceReload, bool) {
	if running == nil || next == nil {
		return nil, false
	}
	if !configContentEqual(stripUnits(running), stripUnits(next)) || staleClaimDurationDiffers(running, next) {
		return nil, false
	}
	runningUnits, err := splitUnits(running)
	if err != nil {
		return nil, false
	}
	nextUnits, err := splitUnits(next)
	if err != nil {
		return nil, false
	}
	plan := &InPlaceReload{
		running: running,
		next:    next,
		retire:  unitsMissingFrom(runningUnits, nextUnits),
		add:     unitsMissingFrom(nextUnits, runningUnits),
	}
	for _, u := range slices.Concat(plan.retire, plan.add) {
		if mayAttachHTTPEndpoint(u.sub, transports) {
			return nil, false
		}
	}
	plan.serialized = RequiresSerializedSwap(unionSub(running, plan.retire), unionSub(next, plan.add), transports)
	return plan, true
}

// Serialized reports whether the retired units must stop before the added
// units are built, rather than the added units being built first.
func (r *InPlaceReload) Serialized() bool { return r.serialized }

// Summary lists, sorted, the route and session ids the plan retires and adds.
// An id in both a retired and an added list belongs to a unit that changed.
func (r *InPlaceReload) Summary() (retiredRoutes, addedRoutes, retiredSessions, addedSessions []string) {
	retiredRoutes, retiredSessions = unitMemberIDs(r.retire)
	addedRoutes, addedSessions = unitMemberIDs(r.add)
	return retiredRoutes, addedRoutes, retiredSessions, addedSessions
}

func unitMemberIDs(units []reloadUnit) (routes, sessions []string) {
	for _, u := range units {
		routes = append(routes, u.routes...)
		sessions = append(sessions, u.sessions...)
	}
	slices.Sort(routes)
	slices.Sort(sessions)
	return routes, sessions
}

// unitsMissingFrom returns the units of from whose key is not among other's.
func unitsMissingFrom(from, other []reloadUnit) []reloadUnit {
	keys := make(map[string]bool, len(other))
	for _, u := range other {
		keys[u.key] = true
	}
	var missing []reloadUnit
	for _, u := range from {
		if !keys[u.key] {
			missing = append(missing, u)
		}
	}
	return missing
}

// staleClaimDurationDiffers reports whether running and next hand their outbox
// store a different stale-claim duration. A duration that cannot be derived
// counts as a difference, so the caller falls back to the full replacement,
// whose build reports the error.
func staleClaimDurationDiffers(running, next *ports.BridgeConfig) bool {
	a, aok, aerr := derivedStaleClaimDuration(running)
	b, bok, berr := derivedStaleClaimDuration(next)
	return aerr != nil || berr != nil || aok != bok || a != b
}

// mayAttachHTTPEndpoint reports whether sub attaches anything — a receiver, a
// sender or a session something references — to a transport whose factory
// advertises ports.CapHTTPEndpoint, or to one transports has no factory for:
// its capabilities cannot be read, so it counts as one that mounts.
func mayAttachHTTPEndpoint(sub *ports.BridgeConfig, transports map[string]ports.TransportFactory) bool {
	return slices.ContainsFunc(attachedTransportKinds(sub), func(kind string) bool {
		tf := transports[kind]
		return tf == nil || hasTransportCapability(tf, ports.CapHTTPEndpoint)
	})
}
