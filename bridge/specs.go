package bridge

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// sessionSpecFrom converts a ports.SessionDef to a ports.SessionSpec.
// The typed Config produced by the two-stage parser is forwarded
// verbatim; the bridge does no further decoding.
func sessionSpecFrom(def ports.SessionDef) ports.SessionSpec {
	return ports.SessionSpec{
		ID:          def.ID,
		Transport:   def.Transport,
		SessionMode: connectivity.SessionMode(def.SessionMode),
		Config:      def.Config,
	}
}

func sessionSpecWithManagedSubscriptions(def ports.SessionDef, cfg *ports.BridgeConfig, store ports.ManagedSubscriptionStore) (ports.SessionSpec, error) {
	spec := sessionSpecFrom(def)
	mode := connectivity.SessionMode(def.SessionMode)
	if !isMQTTPahoTransport(def.Transport) || (mode != connectivity.SessionPersistent && mode != connectivity.SessionExclusive) {
		return spec, nil
	}
	// A configured store is also injected for an EMPTY desired plan: this is
	// how a replacement removes filters remembered by the prior runtime. With
	// no desired subscriptions and no store there is no managed ownership.
	if !sessionHasDesiredSubscriptions(cfg, def.ID) && store == nil {
		return spec, nil
	}
	if store == nil {
		return ports.SessionSpec{}, fmt.Errorf("bridge: persistent/exclusive MQTT session with desired subscriptions requires stores.managed_subscriptions")
	}
	identityConfig, ok := def.Config.(ports.DurableSessionIdentityConfig)
	if !ok || ports.IsNilPluginConfig(def.Config) {
		return ports.SessionSpec{}, fmt.Errorf("bridge: persistent/exclusive MQTT session config does not expose a durable storage identity")
	}
	identity, err := identityConfig.DurableSessionIdentity(mode)
	if err != nil {
		return ports.SessionSpec{}, fmt.Errorf("bridge: derive managed subscription storage identity: %w", err)
	}
	if identity == "" {
		return ports.SessionSpec{}, fmt.Errorf("bridge: managed subscription storage identity is empty")
	}
	spec.ManagedSubscriptionStore = store
	spec.ManagedSubscriptionIdentity = identity
	spec.ManagedSubscriptionsRequired = true
	return spec, nil
}

func isMQTTPahoTransport(kind string) bool {
	return kind == "mqtt" || kind == "mqtt.paho"
}

func sessionHasDesiredSubscriptions(cfg *ports.BridgeConfig, sessionID string) bool {
	if cfg == nil {
		return false
	}
	for i := range cfg.Receivers {
		if cfg.Receivers[i].SessionID == sessionID && len(cfg.Receivers[i].Topics) > 0 {
			return true
		}
	}
	return false
}

func requiresManagedSubscriptionStore(cfg *ports.BridgeConfig) bool {
	if cfg == nil {
		return false
	}
	for i := range cfg.Sessions {
		def := &cfg.Sessions[i]
		mode := connectivity.SessionMode(def.SessionMode)
		if isMQTTPahoTransport(def.Transport) && (mode == connectivity.SessionPersistent || mode == connectivity.SessionExclusive) && sessionHasDesiredSubscriptions(cfg, def.ID) {
			return true
		}
	}
	return false
}

// receiverSpecFrom converts a ports.ReceiverDef to a ports.ReceiverSpec.
// SubscriptionDef entries are mapped to connectivity.SubscriptionPlan values.
func receiverSpecFrom(def ports.ReceiverDef) ports.ReceiverSpec {
	var subs []connectivity.SubscriptionPlan
	if len(def.Topics) > 0 {
		subs = make([]connectivity.SubscriptionPlan, len(def.Topics))
		for i, t := range def.Topics {
			subs[i] = connectivity.SubscriptionPlan{
				Topic:  t.Topic,
				QoS:    t.QoS,
				Config: t.Config,
			}
		}
	}
	return ports.ReceiverSpec{
		ID:            def.ID,
		SessionID:     def.SessionID,
		Subscriptions: subs,
		Config:        def.Config,
	}
}

// senderSpecFrom converts a ports.SenderDef to a ports.SenderSpec.
func senderSpecFrom(def ports.SenderDef) ports.SenderSpec {
	return ports.SenderSpec{
		ID:        def.ID,
		SessionID: def.SessionID,
		Config:    def.Config,
	}
}

// sessionPlanFor assembles the desired connectivity.SessionPlan for the
// session identified by sessionID. The plan is the per-session UNION of
// every receiver bound to that session (ReceiverDef.SessionID ==
// sessionID), mapping each receiver's topics through the SAME
// ReceiverDef.Topics -> SubscriptionPlan conversion that receiverSpecFrom
// applies to the ReceiverSpec, so the adapter sees identical typed
// per-subscription Config on both the spec and the reconcile plan.
//
// It is computed deterministically from the config (receiver declaration
// order for subscriptions; sorted, deduplicated receiver IDs) and independent
// of which route triggers it, so every route
// sharing the session derives an identical plan. That keeps the runtime's
// first-wins session-manager dedup (runtime/bridge_start.go) safe:
// whichever route's sessCfg the manager is built from carries the same
// subscriptions.
//
// Without this the broker session reconciles an empty plan. That is the
// production blocker for the PLAN-DRIVEN transports — MQTT (paho) and
// amqp091 — which establish their subscriptions/topology ONLY from
// plan.Subscriptions in Session.Reconcile, so an empty plan subscribes to
// nothing. amqp10 receivers self-establish links lazily on start (the plan
// feeds only Health) and Service Bus ignores the session entirely, so those
// two are unaffected by the empty-plan defect; the union is still assembled
// uniformly for every transport at no cost.
//
// ponytail: Publishers is the per-session set of sender exchanges advertised
// via ports.PublishingConfig, deduped by exchange name. A sender whose typed
// config doesn't implement the interface, or whose exchange is empty,
// contributes nothing (MQTT / SQS / ASB publish directly to an address and need
// no pre-declaration). This threads the exchange name into PublisherPlan.Topic
// so amqp091's declarePublisher best-effort declares the publish-side exchange.
//
// When two senders name the SAME exchange the FIRST wins (matching the broker's
// first-declare-wins). If a later sibling declares a GENUINELY DIFFERENT
// topology (PublisherTopologyKey mismatch) the divergence is a misconfig that
// would otherwise vanish silently, so it is logged via logger; an identical
// re-declaration is legitimate fan-out and stays silent (REV-2-topowarn). logger
// may be nil (no warning is emitted then).
func sessionPlanFor(cfg *ports.BridgeConfig, sessionID string, logger *slog.Logger) connectivity.SessionPlan {
	if cfg == nil || sessionID == "" {
		return connectivity.SessionPlan{}
	}
	var subs []connectivity.SubscriptionPlan
	receiverIDs := make(map[string]struct{})
	for i := range cfg.Receivers {
		rd := cfg.Receivers[i]
		if rd.SessionID != sessionID {
			continue
		}
		receiverIDs[rd.ID] = struct{}{}
		// Reuse the exact receiverSpecFrom mapping so the spec and the
		// reconcile plan never drift.
		subs = append(subs, receiverSpecFrom(rd).Subscriptions...)
	}
	expectedReceiverIDs := make([]string, 0, len(receiverIDs))
	for id := range receiverIDs {
		expectedReceiverIDs = append(expectedReceiverIDs, id)
	}
	sort.Strings(expectedReceiverIDs)
	var pubs []connectivity.PublisherPlan
	// kept records, per exchange name, the FIRST sender that declared it so a
	// later sibling naming the same exchange can be compared against it.
	type keptPublisher struct {
		senderID string
		decl     ports.PublishingConfig
	}
	kept := make(map[string]keptPublisher)
	for i := range cfg.Senders {
		sd := cfg.Senders[i]
		if sd.SessionID != sessionID {
			continue
		}
		decl, ok := sd.Config.(ports.PublishingConfig)
		if !ok {
			continue
		}
		topic := decl.PublisherTopic()
		if topic == "" {
			// No exchange to declare (MQTT / SQS / ASB publish directly to an
			// address and need no pre-declaration).
			continue
		}
		if first, dup := kept[topic]; dup {
			// Dedup by exchange name, keeping the FIRST sender's Config — matching
			// the broker's own first-declare-wins semantics (a second, conflicting
			// declare would PRECONDITION_FAIL) and the runtime's first-wins session
			// dedup. Warn ONLY when the collapsed sibling's declared topology
			// genuinely differs from the kept first: an identical re-declaration is
			// a legitimate fan-out and must stay silent (REV-2-topowarn).
			if logger != nil && decl.PublisherTopologyKey() != first.decl.PublisherTopologyKey() {
				logger.Warn("duplicate publisher exchange declared with divergent topology; keeping the first declaration and ignoring the later one",
					"session", sessionID,
					"exchange", topic,
					"kept_sender", first.senderID,
					"ignored_sender", sd.ID,
					"kept_topology", first.decl.PublisherTopologyKey(),
					"ignored_topology", decl.PublisherTopologyKey(),
				)
			}
			continue
		}
		kept[topic] = keptPublisher{senderID: sd.ID, decl: decl}
		pubs = append(pubs, connectivity.PublisherPlan{Topic: topic, Config: sd.Config})
	}
	return connectivity.SessionPlan{
		Subscriptions:       subs,
		Publishers:          pubs,
		ExpectedReceiverIDs: expectedReceiverIDs,
	}
}

// Package bridge specs.go intentionally omits a StoreSpec converter:
// the bridge passes the typed PluginConfig from
// ports.StoreConfig directly to the StoreFactory and threads outbox
// runtime tuning (stale claim duration) through ports.OutboxRuntimeOptions.
