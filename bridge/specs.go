package bridge

import (
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
// order) and independent of which route triggers it, so every route
// sharing the session derives an identical plan. That keeps the runtime's
// first-wins session-manager dedup (runtime/bridge_start.go) safe:
// whichever route's sessCfg the manager is built from carries the same
// subscriptions.
//
// Without this the broker session reconciles an empty plan. That is the
// F1 production blocker for the PLAN-DRIVEN transports — MQTT (paho) and
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
// so amqp091's declarePublisher best-effort declares the publish-side exchange (F1-P3).
func sessionPlanFor(cfg *ports.BridgeConfig, sessionID string) connectivity.SessionPlan {
	if cfg == nil || sessionID == "" {
		return connectivity.SessionPlan{}
	}
	var subs []connectivity.SubscriptionPlan
	for i := range cfg.Receivers {
		rd := cfg.Receivers[i]
		if rd.SessionID != sessionID {
			continue
		}
		// Reuse the exact receiverSpecFrom mapping so the spec and the
		// reconcile plan never drift.
		subs = append(subs, receiverSpecFrom(rd).Subscriptions...)
	}
	var pubs []connectivity.PublisherPlan
	seen := make(map[string]bool)
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
		if topic == "" || seen[topic] {
			// Dedup by exchange name, keeping the FIRST sender's Config. Two
			// senders naming the same exchange with divergent publisher.*
			// topology collapse to the first — matching the broker's own
			// first-declare-wins semantics (a second, conflicting declare would
			// PRECONDITION_FAIL) and the runtime's first-wins session dedup.
			continue
		}
		seen[topic] = true
		pubs = append(pubs, connectivity.PublisherPlan{Topic: topic, Config: sd.Config})
	}
	return connectivity.SessionPlan{Subscriptions: subs, Publishers: pubs}
}

// Package bridge specs.go intentionally omits a StoreSpec converter:
// post-PHASE3 the bridge passes the typed PluginConfig from
// ports.StoreConfig directly to the StoreFactory and threads outbox
// runtime tuning (stale claim duration) through ports.OutboxRuntimeOptions.
