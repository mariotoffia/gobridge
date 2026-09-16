package bridge

import (
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

type bestEffortVerdictConfig struct{ redeliveryVerdictConfig }

func (c *bestEffortVerdictConfig) BestEffortDirectHoldTopics(
	session ports.SessionSpec, subscriptions []connectivity.SubscriptionPlan,
) ([]string, string) {
	c.sawSession = session.ID
	return []string{subscriptions[0].Topic}, ""
}

var _ ports.BestEffortDirectHoldConfig = (*bestEffortVerdictConfig)(nil)
