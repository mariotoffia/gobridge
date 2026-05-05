package amqp091

import "github.com/mariotoffia/gobridge/domain"

// subscriptionParams extracts exchange/routing_key/exchange_type/
// durable/auto_delete from a SubscriptionPlan. The values are read
// from the typed Config attached to SubscriptionPlan.Config (post
// PHASE3 there is no legacy Options carrier).
func subscriptionParams(sub domain.SubscriptionPlan) (exchange, routingKey, exchangeType string, durable, autoDelete bool) {
	exchangeType = "direct"
	cfg, ok := configFromPlan(sub.Config)
	if !ok {
		return
	}
	p := cfg.Subscription
	if p.ExchangeType != "" {
		exchangeType = p.ExchangeType
	}
	return p.Exchange, p.RoutingKey, exchangeType, p.Durable, p.AutoDelete
}

// publisherParams mirrors subscriptionParams for PublisherPlan.
func publisherParams(pub domain.PublisherPlan) (exchangeType string, durable, autoDelete bool) {
	exchangeType = "direct"
	cfg, ok := configFromPlan(pub.Config)
	if !ok {
		return
	}
	p := cfg.Publisher
	if p.ExchangeType != "" {
		exchangeType = p.ExchangeType
	}
	return exchangeType, p.Durable, p.AutoDelete
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
