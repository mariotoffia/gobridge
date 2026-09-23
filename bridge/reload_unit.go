package bridge

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mariotoffia/gobridge/ports"
)

// reloadUnit is one connected component of a configuration: the sessions,
// receivers, senders, bindings and routes joined by every reference the
// builder follows — a receiver's, sender's or binding's session_id, a
// binding's sender_id, and a route's receiver_id, bindings and session block.
// Processors are global and are part of a route's content, not members.
//
// An in-place reload replaces whole units. A unit whose key is in both
// documents keeps running untouched; any change inside a unit retires the old
// unit and adds the new one.
//
// ponytail: the unit is the smallest thing an in-place reload replaces. Two
// routes that share a session are one unit, so changing either one reconnects
// that shared session. Replacing a single route on a live session needs a
// receiver rebuilt against a running session; add it when a shared session per
// tenant becomes the common shape.
type reloadUnit struct {
	// key names the unit's content: the hex sha256 of sub's canonical bytes
	// (content normal form, ADR 0016). Two units are the same iff their keys
	// are equal, so an unchanged unit has one key in both documents.
	key string
	// routes are the unit's route ids, sorted.
	routes []string
	// sessions are every session id the runtime can have registered for the
	// unit, sorted: the declared sessions and every id a route session block,
	// binding, receiver or sender names, whether declared or not.
	sessions []string
	// sub is the source config's bridge-wide sections plus exactly this unit's
	// members, in their original relative order: a config the Builder can
	// build on its own. It shares plugin configs with the source config.
	sub *ports.BridgeConfig
	// httpEndpoint reports that a member attaches to a transport whose factory
	// advertises ports.CapHTTPEndpoint. PlanInPlaceReload sets it.
	httpEndpoint bool
}

// splitUnits partitions cfg's sessions, receivers, senders, bindings and
// routes into reload units, ordered by where each unit's first member appears.
//
// A reference to a receiver, sender or binding cfg does not declare adds no
// edge; validation rejects such a document anyway. A session id is a node
// whether declared or not, because the runtime registers a manager under an id
// a route session block or binding names, and retiring the unit must name it.
//
// It fails only when a unit's sub-config cannot be canonicalised.
func splitUnits(cfg *ports.BridgeConfig) ([]reloadUnit, error) {
	if cfg == nil {
		return nil, nil
	}
	g := linkUnitMembers(cfg)

	byRoot := make(map[string]*reloadUnit)
	var order []*reloadUnit
	unitOf := func(node string) *reloadUnit {
		root := g.find(node)
		u := byRoot[root]
		if u == nil {
			u = &reloadUnit{sub: stripUnits(cfg)}
			byRoot[root] = u
			order = append(order, u)
		}
		return u
	}
	for i := range cfg.Sessions {
		u := unitOf("s:" + cfg.Sessions[i].ID)
		u.sub.Sessions = append(u.sub.Sessions, cfg.Sessions[i])
	}
	for i := range cfg.Receivers {
		u := unitOf("rcv:" + cfg.Receivers[i].ID)
		u.sub.Receivers = append(u.sub.Receivers, cfg.Receivers[i])
	}
	for i := range cfg.Senders {
		u := unitOf("snd:" + cfg.Senders[i].ID)
		u.sub.Senders = append(u.sub.Senders, cfg.Senders[i])
	}
	for i := range cfg.Bindings {
		u := unitOf("b:" + cfg.Bindings[i].ID)
		u.sub.Bindings = append(u.sub.Bindings, cfg.Bindings[i])
	}
	for i := range cfg.Routes {
		u := unitOf("r:" + cfg.Routes[i].ID)
		u.sub.Routes = append(u.sub.Routes, cfg.Routes[i])
		u.routes = append(u.routes, cfg.Routes[i].ID)
	}
	// Every session node, declared or only named, joins its referrer's unit,
	// so this finds a unit made above for each one.
	for _, node := range g.order {
		if id, ok := strings.CutPrefix(node, "s:"); ok {
			u := unitOf(node)
			u.sessions = append(u.sessions, id)
		}
	}

	units := make([]reloadUnit, 0, len(order))
	for _, u := range order {
		slices.Sort(u.routes)
		slices.Sort(u.sessions)
		raw, ok := configCanonicalBytes(u.sub)
		if !ok {
			return nil, fmt.Errorf("bridge: reload unit of routes %q and sessions %q cannot be canonicalised", u.routes, u.sessions)
		}
		u.key = candidateConfigDigest(raw)
		units = append(units, *u)
	}
	return units, nil
}

// linkUnitMembers builds the member graph of cfg: one node per declared member
// ("s:", "rcv:", "snd:", "b:", "r:" + id) and per session id anything names,
// joined along every reference the builder follows.
func linkUnitMembers(cfg *ports.BridgeConfig) *unitGraph {
	g := &unitGraph{parent: make(map[string]string)}
	for i := range cfg.Sessions {
		g.add("s:" + cfg.Sessions[i].ID)
	}
	for i := range cfg.Receivers {
		g.add("rcv:" + cfg.Receivers[i].ID)
	}
	for i := range cfg.Senders {
		g.add("snd:" + cfg.Senders[i].ID)
	}
	for i := range cfg.Bindings {
		g.add("b:" + cfg.Bindings[i].ID)
	}
	for i := range cfg.Routes {
		g.add("r:" + cfg.Routes[i].ID)
	}

	for i := range cfg.Receivers {
		g.joinSession("rcv:"+cfg.Receivers[i].ID, cfg.Receivers[i].SessionID)
	}
	for i := range cfg.Senders {
		g.joinSession("snd:"+cfg.Senders[i].ID, cfg.Senders[i].SessionID)
	}
	for i := range cfg.Bindings {
		bd := &cfg.Bindings[i]
		g.joinSession("b:"+bd.ID, bd.SessionID)
		g.join("b:"+bd.ID, "snd:"+bd.SenderID)
	}
	for i := range cfg.Routes {
		rd := &cfg.Routes[i]
		node := "r:" + rd.ID
		g.join(node, "rcv:"+rd.ReceiverID)
		for _, bindingID := range rd.Bindings {
			g.join(node, "b:"+bindingID)
		}
		if rd.Session != nil {
			g.joinSession(node, rd.Session.SessionID)
			g.join(node, "snd:"+rd.Session.SenderID)
		}
	}
	return g
}

// unitGraph is a union-find over member nodes. order keeps the order nodes
// were first added, so a split is deterministic.
type unitGraph struct {
	parent map[string]string
	order  []string
}

func (g *unitGraph) add(node string) {
	if _, ok := g.parent[node]; !ok {
		g.parent[node] = node
		g.order = append(g.order, node)
	}
}

// find returns node's component root. node must have been added.
func (g *unitGraph) find(node string) string {
	for g.parent[node] != node {
		g.parent[node] = g.parent[g.parent[node]]
		node = g.parent[node]
	}
	return node
}

// join puts from and to in one component. A to that was never added is a
// reference to a member the config does not declare, and adds nothing.
func (g *unitGraph) join(from, to string) {
	if _, ok := g.parent[to]; !ok {
		return
	}
	if a, b := g.find(from), g.find(to); a != b {
		g.parent[b] = a
	}
}

// joinSession puts from in the component of the session named id, declared or
// not. An empty id names no session.
func (g *unitGraph) joinSession(from, id string) {
	if id == "" {
		return
	}
	g.add("s:" + id)
	g.join(from, "s:"+id)
}

// stripUnits returns cfg's bridge-wide part: a copy of cfg with the sessions,
// receivers, senders, bindings and routes left out. The copy shares every
// other section with cfg.
func stripUnits(cfg *ports.BridgeConfig) *ports.BridgeConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.Sessions, out.Receivers, out.Senders, out.Bindings, out.Routes = nil, nil, nil, nil, nil
	return &out
}

// unionSub returns base's bridge-wide part plus every member of units: the
// one config that holds all of them, as RequiresSerializedSwap asks about a
// set of units. base must not be nil.
func unionSub(base *ports.BridgeConfig, units []reloadUnit) *ports.BridgeConfig {
	out := stripUnits(base)
	for _, u := range units {
		out.Sessions = append(out.Sessions, u.sub.Sessions...)
		out.Receivers = append(out.Receivers, u.sub.Receivers...)
		out.Senders = append(out.Senders, u.sub.Senders...)
		out.Bindings = append(out.Bindings, u.sub.Bindings...)
		out.Routes = append(out.Routes, u.sub.Routes...)
	}
	return out
}
