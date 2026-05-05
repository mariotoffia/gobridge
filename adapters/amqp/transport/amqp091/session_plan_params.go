package amqp091

import "github.com/mariotoffia/gobridge/domain"

// subscriptionParams extracts exchange/routing_key/exchange_type/
// durable/auto_delete from a SubscriptionPlan. PHASE2 prefers the
// typed Config (Subscription nested params); legacy Options remain
// supported for callers still hand-building plans.
func subscriptionParams(sub domain.SubscriptionPlan) (exchange, routingKey, exchangeType string, durable, autoDelete bool) {
	exchangeType = "direct"
	if cfg, ok := configFromPlan(sub.Config); ok {
		p := cfg.Subscription
		if p.ExchangeType != "" {
			exchangeType = p.ExchangeType
		}
		return p.Exchange, p.RoutingKey, exchangeType, p.Durable, p.AutoDelete
	}
	exchange, _ = optString(sub.Options, "exchange")
	routingKey, _ = optString(sub.Options, "routing_key")
	if et, ok := optString(sub.Options, "exchange_type"); ok {
		exchangeType = et
	}
	durable, _ = optBool(sub.Options, "durable")
	autoDelete, _ = optBool(sub.Options, "auto_delete")
	return
}

// publisherParams mirrors subscriptionParams for PublisherPlan.
func publisherParams(pub domain.PublisherPlan) (exchangeType string, durable, autoDelete bool) {
	exchangeType = "direct"
	if cfg, ok := configFromPlan(pub.Config); ok {
		p := cfg.Publisher
		if p.ExchangeType != "" {
			exchangeType = p.ExchangeType
		}
		return exchangeType, p.Durable, p.AutoDelete
	}
	if et, ok := optString(pub.Options, "exchange_type"); ok {
		exchangeType = et
	}
	durable, _ = optBool(pub.Options, "durable")
	autoDelete, _ = optBool(pub.Options, "auto_delete")
	return
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
