package amqp091

import "github.com/mariotoffia/gobridge/domain/connectivity"

// subscriptionDecl is the resolved broker topology for one inbound
// subscription: exchange/queue/bind identity plus the optional argument
// tables (quorum queues, DLX, TTL, headers-binding match, ...).
type subscriptionDecl struct {
	exchange     string
	routingKey   string
	exchangeType string
	durable      bool
	autoDelete   bool
	queueArgs    map[string]any
	exchangeArgs map[string]any
	bindArgs     map[string]any
}

// subscriptionParams extracts the declaration topology from a
// SubscriptionPlan. The values are read from the typed Config attached
// to SubscriptionPlan.Config (post PHASE3 there is no legacy Options
// carrier).
func subscriptionParams(sub connectivity.SubscriptionPlan) subscriptionDecl {
	d := subscriptionDecl{exchangeType: "direct"}
	cfg, ok := configFromPlan(sub.Config)
	if !ok {
		return d
	}
	p := cfg.Subscription
	if p.ExchangeType != "" {
		d.exchangeType = p.ExchangeType
	}
	d.exchange = p.Exchange
	d.routingKey = p.RoutingKey
	d.durable = p.Durable
	d.autoDelete = p.AutoDelete
	d.queueArgs = p.QueueArguments
	d.exchangeArgs = p.ExchangeArguments
	d.bindArgs = p.BindingArguments
	return d
}

// publisherDecl mirrors subscriptionDecl for an outbound publisher,
// which only declares an exchange.
type publisherDecl struct {
	exchangeType string
	durable      bool
	autoDelete   bool
	exchangeArgs map[string]any
}

// publisherParams mirrors subscriptionParams for PublisherPlan.
func publisherParams(pub connectivity.PublisherPlan) publisherDecl {
	d := publisherDecl{exchangeType: "direct"}
	cfg, ok := configFromPlan(pub.Config)
	if !ok {
		return d
	}
	p := cfg.Publisher
	if p.ExchangeType != "" {
		d.exchangeType = p.ExchangeType
	}
	d.durable = p.Durable
	d.autoDelete = p.AutoDelete
	d.exchangeArgs = p.ExchangeArguments
	return d
}

// configFromPlan accepts both *Config and Config attached to a plan.
func configFromPlan(cfg any) (Config, bool) {
	switch v := cfg.(type) {
	case *Config:
		if v == nil {
			return Config{}, false
		}
		return *v, true
	case Config:
		return v, true
	default:
		return Config{}, false
	}
}
