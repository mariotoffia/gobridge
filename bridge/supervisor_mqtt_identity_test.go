package bridge

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type durableIdentityTestConfig struct {
	identity string
	err      error
}

func (durableIdentityTestConfig) Kind() string    { return "identity" }
func (durableIdentityTestConfig) Validate() error { return nil }
func (c durableIdentityTestConfig) DurableSessionIdentity(connectivity.SessionMode) (string, error) {
	return c.identity, c.err
}

func configWithDurableSessionIdentity(version int, identity string) *ports.BridgeConfig {
	cfg := supervisorTestConfig("r1")
	cfg.Version = version
	cfg.Sessions = []ports.SessionDef{{
		ID:          "stable-session",
		Transport:   "identity",
		SessionMode: "persistent",
		Config:      durableIdentityTestConfig{identity: identity},
	}}
	cfg.Senders[0].Transport = "identity"
	cfg.Senders[0].SessionID = "stable-session"
	cfg.Bindings[0].SessionID = "stable-session"
	return cfg
}

func TestDurableSessionIdentityChanged_StableSessionIDs(t *testing.T) {
	oldCfg := configWithDurableSessionIdentity(1, "opaque-a")

	unchanged := configWithDurableSessionIdentity(2, "opaque-a")
	require.NoError(t, durableSessionIdentityChanged(oldCfg, unchanged))

	changed := configWithDurableSessionIdentity(2, "opaque-b")
	err := durableSessionIdentityChanged(oldCfg, changed)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stable-session")
	assert.NotContains(t, err.Error(), "opaque-a")
	assert.NotContains(t, err.Error(), "opaque-b")

	renamed := configWithDurableSessionIdentity(2, "opaque-b")
	renamed.Sessions[0].ID = "new-session"
	require.Error(t, durableSessionIdentityChanged(oldCfg, renamed), "renaming a durable session can strand its broker state")
}

func TestDurableSessionIdentityChanged_FailsClosedOnCapabilityError(t *testing.T) {
	oldCfg := configWithDurableSessionIdentity(1, "opaque-a")
	newCfg := configWithDurableSessionIdentity(2, "opaque-a")
	newCfg.Sessions[0].Config = durableIdentityTestConfig{err: errors.New("cannot resolve effective identity")}

	err := durableSessionIdentityChanged(oldCfg, newCfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stable-session")
	assert.NotContains(t, err.Error(), "cannot resolve effective identity")
}

func TestSupervisor_SessionIdentityChangeRefusedBeforeBuildAndOldRuntimeContinues(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "destructive override"}[allow], func(t *testing.T) {
			onSwap, swaps := swapChan(1)
			factory := &countingTransportFactory{}
			s := NewSupervisor(WithOnSwap(onSwap), WithAllowDestructiveReload(allow))
			s.RegisterTransport("fake", &fakeTransportFactory{})
			s.RegisterTransport("identity", factory)

			oldCfg := configWithDurableSessionIdentity(1, "opaque-a")
			changes := make(chan *ports.BridgeConfig, 1)
			cancel, errCh := quickSupervisorRun(s, oldCfg, changes)
			defer func() { cancel(); <-errCh }()

			oldRuntime := s.Runtime()
			require.NotNil(t, oldRuntime)
			beforeSessions, _, _ := factory.Counts()
			require.Equal(t, 1, beforeSessions)

			newCfg := configWithDurableSessionIdentity(2, "opaque-b")
			require.True(t, sendConfig(changes, newCfg, time.Second))
			ev := awaitSwap(t, swaps)
			require.Error(t, ev.Error)
			assert.Same(t, oldRuntime, s.Runtime(), "refusal must leave the old runtime serving")
			assert.Same(t, oldCfg, s.Config(), "refusal must preserve old config and version")
			afterSessions, _, _ := factory.Counts()
			assert.Equal(t, beforeSessions, afterSessions, "replacement must not be built before identity preflight")
		})
	}
}
