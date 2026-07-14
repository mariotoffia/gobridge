package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type durableIdentityTestConfig struct {
	identity string
	domains  []string
	err      error
}

func (durableIdentityTestConfig) Kind() string    { return "identity" }
func (durableIdentityTestConfig) Validate() error { return nil }
func (c durableIdentityTestConfig) DurableSessionIdentity(connectivity.SessionMode) (string, error) {
	return c.identity, c.err
}

func (c durableIdentityTestConfig) DurableSessionIdentityDomains(connectivity.SessionMode) ([]string, error) {
	if c.domains != nil {
		return c.domains, c.err
	}
	return []string{c.identity}, c.err
}
func (c durableIdentityTestConfig) FreezePluginConfig() ports.PluginConfig {
	frozen := c
	frozen.domains = append([]string(nil), c.domains...)
	return frozen
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
			assert.Equal(t, oldCfg, s.Config(), "refusal must preserve old config and version")
			assert.Equal(t, 1, s.Config().Version)
			afterSessions, _, _ := factory.Counts()
			assert.Equal(t, beforeSessions, afterSessions, "replacement must not be built before identity preflight")
		})
	}
}

func TestSupervisor_InPlaceSessionIdentityMutationUsesAppliedSnapshot(t *testing.T) {
	onSwap, swaps := swapChan(1)
	factory := &countingTransportFactory{}
	s := NewSupervisor(WithOnSwap(onSwap))
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("identity", factory)

	cfg := configWithDurableSessionIdentity(1, "opaque-a")
	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, cfg, changes)
	defer func() { cancel(); <-errCh }()

	oldRuntime := s.Runtime()
	require.NotNil(t, oldRuntime)
	beforeSessions, _, _ := factory.Counts()

	// Mutate the caller-held object that Supervisor previously retained directly,
	// then submit that same pointer as a reload.
	cfg.Version = 2
	cfg.Sessions[0].Config = durableIdentityTestConfig{identity: "opaque-b"}
	require.True(t, sendConfig(changes, cfg, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Same(t, oldRuntime, s.Runtime())
	require.NotNil(t, s.Config())
	assert.Equal(t, 1, s.Config().Version, "caller mutation must not alter the applied blueprint snapshot")
	afterSessions, _, _ := factory.Counts()
	assert.Equal(t, beforeSessions, afterSessions, "identity refusal must happen before replacement build")
}

func TestDurableSessionIdentityChanged_RejectsDuplicateIdentityOnStartupAndReload(t *testing.T) {
	duplicate := configWithDurableSessionIdentity(2, "opaque-a")
	duplicate.Sessions[0].Config = durableIdentityTestConfig{identity: "opaque-a", domains: []string{"shared-domain"}}
	duplicate.Sessions = append(duplicate.Sessions, ports.SessionDef{
		ID: "duplicate-session", Transport: "identity", SessionMode: "persistent",
		Config: durableIdentityTestConfig{identity: "opaque-b", domains: []string{"shared-domain"}},
	})
	duplicate.Senders = append(duplicate.Senders, ports.SenderDef{
		ID: "duplicate-sender", Transport: "identity", SessionID: "duplicate-session",
	})

	require.Error(t, durableSessionIdentityChanged(nil, duplicate),
		"initial startup must reject duplicate effective identities")

	oldCfg := configWithDurableSessionIdentity(1, "opaque-a")
	require.Error(t, durableSessionIdentityChanged(oldCfg, duplicate),
		"reload must reject a newly-added duplicate effective identity")
}

func TestSupervisor_DuplicateDurableIdentityRejectedBeforeInitialBuild(t *testing.T) {
	factory := &countingTransportFactory{}
	s := NewSupervisor()
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("identity", factory)

	cfg := configWithDurableSessionIdentity(1, "opaque-a")
	cfg.Sessions[0].Config = durableIdentityTestConfig{identity: "opaque-a", domains: []string{"shared-domain"}}
	cfg.Sessions = append(cfg.Sessions, ports.SessionDef{
		ID: "duplicate-session", Transport: "identity", SessionMode: "persistent",
		Config: durableIdentityTestConfig{identity: "opaque-b", domains: []string{"shared-domain"}},
	})
	cfg.Senders = append(cfg.Senders, ports.SenderDef{
		ID: "duplicate-sender", Transport: "identity", SessionID: "duplicate-session",
	})

	err := s.Run(t.Context(), cfg, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate effective broker identities")
	sessions, receivers, senders := factory.Counts()
	assert.Zero(t, sessions)
	assert.Zero(t, receivers)
	assert.Zero(t, senders)
	assert.Nil(t, s.Runtime())
	assert.Nil(t, s.Config())
}

func TestSupervisor_DuplicateDurableIdentityReloadRejectedBeforeBuild(t *testing.T) {
	onSwap, swaps := swapChan(1)
	factory := &countingTransportFactory{}
	s := NewSupervisor(WithOnSwap(onSwap))
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("identity", factory)

	oldCfg := configWithDurableSessionIdentity(1, "opaque-a")
	oldCfg.Sessions[0].Config = durableIdentityTestConfig{identity: "opaque-a", domains: []string{"shared-domain"}}
	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, oldCfg, changes)
	defer func() { cancel(); <-errCh }()

	oldRuntime := s.Runtime()
	beforeSessions, _, _ := factory.Counts()
	newCfg := configWithDurableSessionIdentity(2, "opaque-a")
	newCfg.Sessions[0].Config = durableIdentityTestConfig{identity: "opaque-a", domains: []string{"shared-domain"}}
	newCfg.Sessions = append(newCfg.Sessions, ports.SessionDef{
		ID: "duplicate-session", Transport: "identity", SessionMode: "persistent",
		Config: durableIdentityTestConfig{identity: "different-state", domains: []string{"shared-domain"}},
	})
	newCfg.Senders = append(newCfg.Senders, ports.SenderDef{
		ID: "duplicate-sender", Transport: "identity", SessionID: "duplicate-session",
	})
	require.True(t, sendConfig(changes, newCfg, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "duplicate effective broker identities")
	assert.Same(t, oldRuntime, s.Runtime())
	assert.Equal(t, 1, s.Config().Version)
	afterSessions, _, _ := factory.Counts()
	assert.Equal(t, beforeSessions, afterSessions, "duplicate refusal must precede replacement build")
}

type typedNilDurableIdentityConfig struct{}

func (*typedNilDurableIdentityConfig) Kind() string    { return "identity" }
func (*typedNilDurableIdentityConfig) Validate() error { return nil }
func (*typedNilDurableIdentityConfig) DurableSessionIdentity(connectivity.SessionMode) (string, error) {
	panic("typed nil durable identity invoked")
}

func (*typedNilDurableIdentityConfig) DurableSessionIdentityDomains(connectivity.SessionMode) ([]string, error) {
	panic("typed nil durable identity domains invoked")
}

func TestSnapshotDurableSessionIdentities_RejectsOverlappingEndpointDomainsOnly(t *testing.T) {
	cfg := configWithDurableSessionIdentity(1, "state-a")
	cfg.Sessions[0].Config = durableIdentityTestConfig{
		identity: "state-a", domains: []string{"endpoint-a", "endpoint-b"},
	}
	cfg.Sessions = append(cfg.Sessions, ports.SessionDef{
		ID: "second-session", Transport: "identity", SessionMode: "persistent",
		Config: durableIdentityTestConfig{
			identity: "state-b", domains: []string{"endpoint-a", "endpoint-c"},
		},
	})
	cfg.Senders = append(cfg.Senders, ports.SenderDef{
		ID: "second-sender", Transport: "identity", SessionID: "second-session",
	})

	_, err := snapshotDurableSessionIdentities(cfg)
	require.Error(t, err, "one overlapping broker endpoint plus client identity must collide")

	cfg.Sessions[1].Config = durableIdentityTestConfig{
		identity: "state-b", domains: []string{"endpoint-c", "endpoint-d"},
	}
	_, err = snapshotDurableSessionIdentities(cfg)
	require.NoError(t, err, "non-overlapping broker endpoints must not collide")
}

func TestSnapshotDurableSessionIdentities_OnlyReferencedDurableSessions(t *testing.T) {
	cfg := configWithDurableSessionIdentity(1, "referenced-durable")
	cfg.Sessions = append(cfg.Sessions,
		ports.SessionDef{
			ID: "unreferenced-persistent", Transport: "identity", SessionMode: "persistent",
			Config: durableIdentityTestConfig{err: errors.New("must not be evaluated")},
		},
		ports.SessionDef{
			ID: "referenced-ephemeral", Transport: "identity", SessionMode: "ephemeral",
			Config: durableIdentityTestConfig{err: errors.New("must not be evaluated")},
		},
	)
	cfg.Senders = append(cfg.Senders, ports.SenderDef{
		ID: "ephemeral-sender", Transport: "identity", SessionID: "referenced-ephemeral",
	})

	snapshot, err := snapshotDurableSessionIdentities(cfg)
	require.NoError(t, err)
	assert.Equal(t, durableSessionIdentitySnapshot{
		"stable-session": {kind: "identity", fingerprint: "referenced-durable"},
	}, snapshot)
}

func TestSnapshotDurableSessionIdentities_TypedNilCapabilityReturnsError(t *testing.T) {
	cfg := configWithDurableSessionIdentity(1, "unused")
	var typedNil *typedNilDurableIdentityConfig
	cfg.Sessions[0].Config = typedNil

	var err error
	require.NotPanics(t, func() {
		_, err = snapshotDurableSessionIdentities(cfg)
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stable-session")
}

type opaqueRuntimeDependency struct {
	mu     sync.Mutex
	client *struct{ name string }
}

type mutableIdentityTestConfig struct {
	identityParts []string
	dependency    *opaqueRuntimeDependency

	domainStarted  chan struct{}
	domainContinue chan struct{}
}

func (*mutableIdentityTestConfig) Kind() string    { return "identity" }
func (*mutableIdentityTestConfig) Validate() error { return nil }
func (*mutableIdentityTestConfig) DurableSessionIdentity(connectivity.SessionMode) (string, error) {
	return "stable-state", nil
}
func (c *mutableIdentityTestConfig) ownershipDomains() ([]string, error) {
	domain := c.identityParts[0]
	if c.domainStarted != nil {
		close(c.domainStarted)
		<-c.domainContinue
	}
	return []string{domain}, nil
}
func (c *mutableIdentityTestConfig) DurableSessionIdentityDomains(connectivity.SessionMode) ([]string, error) {
	return c.ownershipDomains()
}

// FreezePluginConfig is adapter-owned: private identity state becomes
// deep-owned while the opaque mutex/client dependency deliberately stays shared.
func (c *mutableIdentityTestConfig) FreezePluginConfig() ports.PluginConfig {
	frozen := *c
	frozen.identityParts = append([]string(nil), c.identityParts...)
	return &frozen
}

type unfreezableDurableIdentityConfig struct{ identity string }

func (unfreezableDurableIdentityConfig) Kind() string    { return "identity" }
func (unfreezableDurableIdentityConfig) Validate() error { return nil }
func (c unfreezableDurableIdentityConfig) DurableSessionIdentity(connectivity.SessionMode) (string, error) {
	return c.identity, nil
}
func (c unfreezableDurableIdentityConfig) DurableSessionIdentityDomains(connectivity.SessionMode) ([]string, error) {
	return []string{c.identity}, nil
}

func TestSupervisor_FreezesProposalBeforeIdentityPreflightAndBuild(t *testing.T) {
	onSwap, swaps := swapChan(1)
	type capturedConfig struct {
		identity   string
		dependency *opaqueRuntimeDependency
	}
	captured := make(chan capturedConfig, 2)
	factory := &countingTransportFactory{SessionFn: func(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
		cfg := spec.Config.(*mutableIdentityTestConfig)
		captured <- capturedConfig{identity: cfg.identityParts[0], dependency: cfg.dependency}
		return &fakeSession{}, nil
	}}
	s := NewSupervisor(WithOnSwap(onSwap))
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("identity", factory)

	dependency := &opaqueRuntimeDependency{client: &struct{ name string }{name: "shared-client"}}
	oldCfg := configWithDurableSessionIdentity(1, "stable-state")
	oldCfg.Sessions[0].Config = &mutableIdentityTestConfig{
		identityParts: []string{"broker-a"}, dependency: dependency,
	}
	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, oldCfg, changes)
	defer func() { cancel(); <-errCh }()
	require.Equal(t, capturedConfig{identity: "broker-a", dependency: dependency}, <-captured)

	started := make(chan struct{})
	proceed := make(chan struct{})
	newCfg := configWithDurableSessionIdentity(2, "stable-state")
	proposed := &mutableIdentityTestConfig{
		identityParts: []string{"broker-a"}, dependency: dependency,
		domainStarted: started, domainContinue: proceed,
	}
	newCfg.Sessions[0].Config = proposed
	require.True(t, sendConfig(changes, newCfg, time.Second))
	<-started
	proposed.identityParts[0] = "broker-mutated-after-preflight"
	close(proceed)

	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error)
	built := <-captured
	assert.Equal(t, "broker-a", built.identity, "build must use the adapter-frozen identity state")
	assert.Same(t, dependency, built.dependency, "opaque runtime dependency must not be copied or detached")
	dependency.mu.Lock()
	assert.Equal(t, "shared-client", dependency.client.name)
	dependency.mu.Unlock()
	stored := s.Config().Sessions[0].Config.(*mutableIdentityTestConfig)
	assert.Equal(t, []string{"broker-a"}, stored.identityParts)
	assert.Same(t, dependency, stored.dependency)
}

func TestSnapshotDurableSessionIdentities_RequiresAdapterOwnedFreezeCapability(t *testing.T) {
	cfg := configWithDurableSessionIdentity(1, "state")
	cfg.Sessions[0].Config = unfreezableDurableIdentityConfig{identity: "state"}

	_, err := snapshotDurableSessionIdentities(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "freeze")
	assert.Contains(t, err.Error(), "stable-session")
}
