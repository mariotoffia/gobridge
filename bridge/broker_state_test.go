package bridge

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

func brokerStateTransports(identity ports.TransportFactory) map[string]ports.TransportFactory {
	return map[string]ports.TransportFactory{"identity": identity, "fake": &fakeTransportFactory{}}
}

func TestPlanBrokerStateChange_AChangedIdentityLosesTheOldKeyAndAddsTheNew(t *testing.T) {
	change, err := PlanBrokerStateChange(
		configWithDurableSessionIdentity(1, "opaque-a"),
		configWithDurableSessionIdentity(2, "opaque-b"),
		brokerStateTransports(newBrokerStateFactory()))

	require.NoError(t, err)
	assert.Equal(t, BrokerStateChange{Lost: []string{"stable-session"}, Added: []string{"stable-session"}}, change)
}

func TestPlanBrokerStateChange_RenamingOnlyTheSessionIDChangesNothing(t *testing.T) {
	change, err := PlanBrokerStateChange(
		configWithDurableSessionIdentity(1, "opaque-a"),
		renamedDurableSession(configWithDurableSessionIdentity(2, "opaque-a"), "renamed-session"),
		brokerStateTransports(newBrokerStateFactory()))

	require.NoError(t, err)
	assert.Equal(t, BrokerStateChange{}, change, "the broker identity is still there under another session_id")
}

func TestPlanBrokerStateChange_ARemovedSessionLosesItsKey(t *testing.T) {
	next := supervisorTestConfig("r1")
	next.Version = 2

	change, err := PlanBrokerStateChange(configWithDurableSessionIdentity(1, "opaque-a"), next,
		brokerStateTransports(newBrokerStateFactory()))

	require.NoError(t, err)
	assert.Equal(t, BrokerStateChange{Lost: []string{"stable-session"}}, change)
}

func TestPlanBrokerStateChange_TheSameConfigurationChangesNothing(t *testing.T) {
	cfg := configWithDurableSessionIdentity(1, "opaque-a")
	factory := newBrokerStateFactory()
	factory.keyErr = errors.New("keys are not computed for a configuration compared with itself")

	change, err := PlanBrokerStateChange(cfg, cfg, brokerStateTransports(factory))

	require.NoError(t, err)
	assert.Equal(t, BrokerStateChange{}, change)
}

func TestPlanBrokerStateChange_AnEphemeralSessionHoldsNoKey(t *testing.T) {
	running := configWithDurableSessionIdentity(1, "opaque-a")
	running.Sessions[0].SessionMode = "ephemeral"
	next := configWithDurableSessionIdentity(2, "opaque-b")
	next.Sessions[0].SessionMode = "ephemeral"

	change, err := PlanBrokerStateChange(running, next, brokerStateTransports(newBrokerStateFactory()))

	require.NoError(t, err)
	assert.Equal(t, BrokerStateChange{}, change)
}

func TestPlanBrokerStateChange_ATransportWithoutTheCapabilityHoldsNoKey(t *testing.T) {
	change, err := PlanBrokerStateChange(
		configWithDurableSessionIdentity(1, "opaque-a"),
		configWithDurableSessionIdentity(2, "opaque-b"),
		brokerStateTransports(&countingTransportFactory{}))

	require.NoError(t, err)
	assert.Equal(t, BrokerStateChange{}, change)
}

func TestPlanBrokerStateChange_AKeyThatCannotBeComputedFailsThePlan(t *testing.T) {
	factory := newBrokerStateFactory()
	keyErr := errors.New("broker URL cannot be parsed")
	factory.keyErr = keyErr

	_, err := PlanBrokerStateChange(
		configWithDurableSessionIdentity(1, "opaque-a"),
		configWithDurableSessionIdentity(2, "opaque-b"),
		brokerStateTransports(factory))

	require.ErrorIs(t, err, keyErr)
}
