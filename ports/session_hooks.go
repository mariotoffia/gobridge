package ports

// ManagedSubscriptionIdentityReporter is an optional session capability. A
// session that keeps a durable history of the exact filters it subscribed (a
// persistent or exclusive MQTT session) reports the identity that history is
// stored under, and "" when it keeps none. The identity names one broker-side
// session, so the runtime records it on a removed-subscription dead-letter and
// matches it when the subscription is added back (ADR 0019).
type ManagedSubscriptionIdentityReporter interface {
	ManagedSubscriptionIdentity() string
}

// SubscriptionAddedHookConfigurer is an optional session capability. The
// runtime installs fn during Runtime.Start or Graft, before session goroutines
// begin. The session calls fn after the broker grants a SUBSCRIBE for filters
// that were not in its managed subscription history — a genuinely added
// subscription, not one re-established after a reconnect or a restart. fn must
// return promptly and must not wait on the runtime. A nil fn means no hook.
type SubscriptionAddedHookConfigurer interface {
	SetSubscriptionAddedHook(fn func(filters []string))
}
