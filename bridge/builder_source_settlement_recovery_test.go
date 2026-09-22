package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// A source session that recycles its broker connection to recover stranded
// settlements waits a bounded time for the deliveries the runtime already
// accepted to settle. That wait lives in the transport's typed config and the
// route validator needs it, so the builder reads it off the INGRESS session —
// the one whose recycle a held delivery would have to outlive — and hands it to
// the runtime with the rest of the source facts.

// settlementRecoveryTimingConfig answers the wait the way a real recycling
// transport does: from the mode the session actually runs in, and only for the
// durable modes that recycle at all.
type settlementRecoveryTimingConfig struct {
	wait    time.Duration
	sawMode connectivity.SessionMode
}

func (c *settlementRecoveryTimingConfig) Kind() string    { return "sqs" }
func (c *settlementRecoveryTimingConfig) Validate() error { return nil }

func (c *settlementRecoveryTimingConfig) SettlementRecoveryWait(mode connectivity.SessionMode) time.Duration {
	c.sawMode = mode
	if mode != connectivity.SessionPersistent && mode != connectivity.SessionExclusive {
		return 0
	}
	return c.wait
}

var _ ports.SettlementRecoveryTimingConfig = (*settlementRecoveryTimingConfig)(nil)

// TestSourceRouteFacts_CarryTheIngressSessionSettlementRecoveryWait pins both
// directions: a durable session hands the route its recycle wait, and a session
// that never recycles hands it nothing to check against.
func TestSourceRouteFacts_CarryTheIngressSessionSettlementRecoveryWait(t *testing.T) {
	cases := []struct {
		name        string
		sessionMode string
		want        time.Duration
	}{
		{name: "persistent session recycles", sessionMode: string(connectivity.SessionPersistent),
			want: 240 * time.Second},
		{name: "ephemeral session never recycles", sessionMode: string(connectivity.SessionEphemeral)},
		{name: "an unset mode is ephemeral"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			timing := &settlementRecoveryTimingConfig{wait: 240 * time.Second}
			cfg := directHoldConfigWithVerdict(&redeliveryVerdictConfig{})
			cfg.Sessions[0].SessionMode = tc.sessionMode
			cfg.Sessions[0].Config = timing

			builder := NewBuilder(cfg).RegisterTransportFactory("sqs", &noCapabilityTransport{})
			facts := builder.sourceRouteFacts(&cfg.Receivers[0])

			assert.Equal(t, tc.want, facts.SettlementRecoveryWait)
			assert.Equal(t, connectivity.SessionMode(tc.sessionMode), timing.sawMode,
				"the transport must be asked about the mode the ingress session runs in")
		})
	}
}

// TestBuilder_SendRetryBudgetOutlivingTheRecycleWaitFailsBuild proves the wait
// reaches the runtime validator through the built route, not just the builder's
// own facts: a route whose in-process send retry could still be running when the
// source's recycle gives up is refused before anything is started.
func TestBuilder_SendRetryBudgetOutlivingTheRecycleWaitFailsBuild(t *testing.T) {
	cfg := directHoldConfigWithVerdict(&redeliveryVerdictConfig{})
	cfg.Sessions[0].SessionMode = string(connectivity.SessionPersistent)
	cfg.Sessions[0].Config = &settlementRecoveryTimingConfig{wait: 240 * time.Second}
	cfg.Routes[0].Policy.SendRetryBudget = "220s"
	cfg.Routes[0].Policy.SendTimeout = "30s"

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("sqs", &noCapabilityTransport{}).
		RegisterTransportFactory("sink", &noCapabilityTransport{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "settlement-recovery wait")
}

// TestBuilder_DefaultSendRetryBudgetFitsTheRecycleWait is the accepting
// direction, so the check cannot pass by refusing every durable source.
func TestBuilder_DefaultSendRetryBudgetFitsTheRecycleWait(t *testing.T) {
	cfg := directHoldConfigWithVerdict(&redeliveryVerdictConfig{})
	cfg.Sessions[0].SessionMode = string(connectivity.SessionPersistent)
	cfg.Sessions[0].Config = &settlementRecoveryTimingConfig{wait: 240 * time.Second}

	rt, err := NewBuilder(cfg).
		RegisterTransportFactory("sqs", &noCapabilityTransport{}).
		RegisterTransportFactory("sink", &noCapabilityTransport{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)
}
