package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// durableIdentityTestConfig stands in for a transport whose session claims a
// durable broker identity, such as an MQTT client id. Identity and Domains are
// EXPORTED because a real transport carries them as written options: they are
// part of the config's content, so changing one is a reload the supervisor sees
// rather than a document that says the same thing as the running one.
type durableIdentityTestConfig struct {
	Identity string   `json:"identity"`
	Domains  []string `json:"domains,omitempty"`
	err      error
}

func (durableIdentityTestConfig) Kind() string    { return "identity" }
func (durableIdentityTestConfig) Validate() error { return nil }
func (c durableIdentityTestConfig) DurableSessionIdentity(connectivity.SessionMode) (string, error) {
	return c.Identity, c.err
}

func (c durableIdentityTestConfig) DurableSessionIdentityDomains(connectivity.SessionMode) ([]string, error) {
	if c.Domains != nil {
		return c.Domains, c.err
	}
	return []string{c.Identity}, c.err
}
func (c durableIdentityTestConfig) FreezePluginConfig() ports.PluginConfig {
	frozen := c
	frozen.Domains = append([]string(nil), c.Domains...)
	return frozen
}

func configWithDurableSessionIdentity(version int, identity string) *ports.BridgeConfig {
	cfg := supervisorTestConfig("r1")
	cfg.Version = version
	cfg.Sessions = []ports.SessionDef{{
		ID:          "stable-session",
		Transport:   "identity",
		SessionMode: "persistent",
		Config:      durableIdentityTestConfig{Identity: identity},
	}}
	cfg.Senders[0].Transport = "identity"
	cfg.Senders[0].SessionID = "stable-session"
	cfg.Bindings[0].SessionID = "stable-session"
	return cfg
}

func startIdentitySupervisor(t *testing.T, cfg *ports.BridgeConfig) (*Supervisor, *countingTransportFactory, <-chan SwapEvent, chan *ports.BridgeConfig) {
	t.Helper()
	onSwap, swaps := swapChan(1)
	factory := &countingTransportFactory{}
	s := NewSupervisor(WithOnSwap(onSwap))
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("identity", factory)
	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, cfg, changes)
	t.Cleanup(func() { cancel(); <-errCh })
	return s, factory, swaps, changes
}

func TestSupervisor_AcceptsAReloadThatChangesADurableBrokerIdentity(t *testing.T) {
	s, factory, swaps, changes := startIdentitySupervisor(t, configWithDurableSessionIdentity(1, "opaque-a"))
	beforeSessions, _, _ := factory.Counts()

	require.True(t, sendConfig(changes, configWithDurableSessionIdentity(2, "opaque-b"), time.Second))
	ev := awaitSwap(t, swaps)

	require.NoError(t, ev.Error, "a changed broker identity is an ordinary reload (ADR 0024)")
	assert.Equal(t, 2, s.Config().Version)
	afterSessions, _, _ := factory.Counts()
	assert.Equal(t, beforeSessions+1, afterSessions, "the reload builds the session under its new identity")
}

func TestSupervisor_AcceptsAReloadThatRemovesADurableSession(t *testing.T) {
	s, _, swaps, changes := startIdentitySupervisor(t, configWithDurableSessionIdentity(1, "opaque-a"))
	next := supervisorTestConfig("r1")
	next.Version = 2

	require.True(t, sendConfig(changes, next, time.Second))
	ev := awaitSwap(t, swaps)

	require.NoError(t, ev.Error)
	assert.Equal(t, 2, s.Config().Version)
	assert.Empty(t, s.Config().Sessions)
}

func TestSupervisor_AcceptsAReloadThatRenamesADurableSession(t *testing.T) {
	s, _, swaps, changes := startIdentitySupervisor(t, configWithDurableSessionIdentity(1, "opaque-a"))
	next := renamedDurableSession(configWithDurableSessionIdentity(2, "opaque-a"), "renamed-session")

	require.True(t, sendConfig(changes, next, time.Second))
	ev := awaitSwap(t, swaps)

	require.NoError(t, ev.Error)
	require.Len(t, s.Config().Sessions, 1)
	assert.Equal(t, "renamed-session", s.Config().Sessions[0].ID)
}

func TestSupervisor_CallerMutationDoesNotAlterTheAppliedConfig(t *testing.T) {
	cfg := configWithDurableSessionIdentity(1, "opaque-a")
	s, _, _, _ := startIdentitySupervisor(t, cfg)

	// Mutate the caller-held object the Supervisor was started with.
	cfg.Version = 2
	cfg.Sessions[0].Config = durableIdentityTestConfig{Identity: "opaque-b"}

	applied := s.Config()
	require.NotNil(t, applied)
	assert.Equal(t, 1, applied.Version, "caller mutation must not alter the applied blueprint snapshot")
	assert.Equal(t, durableIdentityTestConfig{Identity: "opaque-a"}, applied.Sessions[0].Config)
}

func TestValidateDurableBrokerIdentities_FailsClosedOnCapabilityError(t *testing.T) {
	cfg := configWithDurableSessionIdentity(1, "opaque-a")
	cfg.Sessions[0].Config = durableIdentityTestConfig{err: errors.New("cannot resolve effective identity")}

	err := validateDurableBrokerIdentities(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stable-session")
	assert.NotContains(t, err.Error(), "cannot resolve effective identity")
}

func TestValidateDurableBrokerIdentities_RejectsDuplicateIdentities(t *testing.T) {
	duplicate := configWithDurableSessionIdentity(2, "opaque-a")
	duplicate.Sessions[0].Config = durableIdentityTestConfig{Identity: "opaque-a", Domains: []string{"shared-domain"}}
	duplicate.Sessions = append(duplicate.Sessions, ports.SessionDef{
		ID: "duplicate-session", Transport: "identity", SessionMode: "persistent",
		Config: durableIdentityTestConfig{Identity: "opaque-b", Domains: []string{"shared-domain"}},
	})
	duplicate.Senders = append(duplicate.Senders, ports.SenderDef{
		ID: "duplicate-sender", Transport: "identity", SessionID: "duplicate-session",
	})

	require.Error(t, validateDurableBrokerIdentities(duplicate))
}

func TestSupervisor_DuplicateDurableIdentityRejectedBeforeInitialBuild(t *testing.T) {
	factory := &countingTransportFactory{}
	s := NewSupervisor()
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("identity", factory)

	cfg := configWithDurableSessionIdentity(1, "opaque-a")
	cfg.Sessions[0].Config = durableIdentityTestConfig{Identity: "opaque-a", Domains: []string{"shared-domain"}}
	cfg.Sessions = append(cfg.Sessions, ports.SessionDef{
		ID: "duplicate-session", Transport: "identity", SessionMode: "persistent",
		Config: durableIdentityTestConfig{Identity: "opaque-b", Domains: []string{"shared-domain"}},
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
	oldCfg.Sessions[0].Config = durableIdentityTestConfig{Identity: "opaque-a", Domains: []string{"shared-domain"}}
	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, oldCfg, changes)
	defer func() { cancel(); <-errCh }()

	oldRuntime := s.Runtime()
	beforeSessions, _, _ := factory.Counts()
	newCfg := configWithDurableSessionIdentity(2, "opaque-a")
	newCfg.Sessions[0].Config = durableIdentityTestConfig{Identity: "opaque-a", Domains: []string{"shared-domain"}}
	newCfg.Sessions = append(newCfg.Sessions, ports.SessionDef{
		ID: "duplicate-session", Transport: "identity", SessionMode: "persistent",
		Config: durableIdentityTestConfig{Identity: "different-state", Domains: []string{"shared-domain"}},
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

func TestValidateDurableBrokerIdentities_RejectsOverlappingEndpointDomainsOnly(t *testing.T) {
	cfg := configWithDurableSessionIdentity(1, "state-a")
	cfg.Sessions[0].Config = durableIdentityTestConfig{
		Identity: "state-a", Domains: []string{"endpoint-a", "endpoint-b"},
	}
	cfg.Sessions = append(cfg.Sessions, ports.SessionDef{
		ID: "second-session", Transport: "identity", SessionMode: "persistent",
		Config: durableIdentityTestConfig{
			Identity: "state-b", Domains: []string{"endpoint-a", "endpoint-c"},
		},
	})
	cfg.Senders = append(cfg.Senders, ports.SenderDef{
		ID: "second-sender", Transport: "identity", SessionID: "second-session",
	})

	err := validateDurableBrokerIdentities(cfg)
	require.Error(t, err, "one overlapping broker endpoint plus client identity must collide")

	cfg.Sessions[1].Config = durableIdentityTestConfig{
		Identity: "state-b", Domains: []string{"endpoint-c", "endpoint-d"},
	}
	err = validateDurableBrokerIdentities(cfg)
	require.NoError(t, err, "non-overlapping broker endpoints must not collide")
}

func TestValidateDurableBrokerIdentities_ChecksOnlyReferencedDurableSessions(t *testing.T) {
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

	require.NoError(t, validateDurableBrokerIdentities(cfg))
}

func TestValidateDurableBrokerIdentities_TypedNilCapabilityReturnsError(t *testing.T) {
	cfg := configWithDurableSessionIdentity(1, "unused")
	var typedNil *typedNilDurableIdentityConfig
	cfg.Sessions[0].Config = typedNil

	var err error
	require.NotPanics(t, func() {
		err = validateDurableBrokerIdentities(cfg)
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
	// domainOnce is shared by the frozen copies, so the hook fires on the first
	// call only, however often validation asks for the domains.
	domainOnce *sync.Once
}

func (*mutableIdentityTestConfig) Kind() string    { return "identity" }
func (*mutableIdentityTestConfig) Validate() error { return nil }
func (*mutableIdentityTestConfig) DurableSessionIdentity(connectivity.SessionMode) (string, error) {
	return "stable-state", nil
}
func (c *mutableIdentityTestConfig) ownershipDomains() ([]string, error) {
	domain := c.identityParts[0]
	if c.domainStarted != nil {
		c.domainOnce.Do(func() {
			close(c.domainStarted)
			<-c.domainContinue
		})
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

func TestSupervisor_FreezesProposalBeforeValidationAndBuild(t *testing.T) {
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
	// The plugin's identity state is private to the adapter, so it is invisible
	// to the content comparison; without a visible edit this document would say
	// the same thing as the running one and no preflight would run at all.
	newCfg.Bindings[0].Address = "addr/reloaded"
	proposed := &mutableIdentityTestConfig{
		identityParts: []string{"broker-a"}, dependency: dependency,
		domainStarted: started, domainContinue: proceed, domainOnce: &sync.Once{},
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

func TestValidateDurableBrokerIdentities_RequiresAdapterOwnedFreezeCapability(t *testing.T) {
	cfg := configWithDurableSessionIdentity(1, "state")
	cfg.Sessions[0].Config = unfreezableDurableIdentityConfig{identity: "state"}

	err := validateDurableBrokerIdentities(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "freeze")
	assert.Contains(t, err.Error(), "stable-session")
}

type freezeOutputConfig struct{ kind string }

func (c freezeOutputConfig) Kind() string                           { return c.kind }
func (freezeOutputConfig) Validate() error                          { return nil }
func (c freezeOutputConfig) FreezePluginConfig() ports.PluginConfig { return c }

type maliciousDurableFreezer struct{ output ports.PluginConfig }

func (maliciousDurableFreezer) Kind() string    { return "identity" }
func (maliciousDurableFreezer) Validate() error { return nil }
func (maliciousDurableFreezer) DurableSessionIdentity(connectivity.SessionMode) (string, error) {
	return "state", nil
}
func (maliciousDurableFreezer) DurableSessionIdentityDomains(connectivity.SessionMode) ([]string, error) {
	return []string{"domain"}, nil
}
func (c maliciousDurableFreezer) FreezePluginConfig() ports.PluginConfig { return c.output }

type nonFreezableCredentialConfig struct {
	credentialsURICalled bool
	applyCalled          bool
}

func (*nonFreezableCredentialConfig) Kind() string    { return "credentialed" }
func (*nonFreezableCredentialConfig) Validate() error { return nil }
func (c *nonFreezableCredentialConfig) CredentialsURI() string {
	c.credentialsURICalled = true
	return "vault://credentials"
}
func (c *nonFreezableCredentialConfig) ApplyCredentials(*connectivity.CredentialSet) error {
	c.applyCalled = true
	return nil
}

func TestFreezePluginConfig_RejectsInvalidAdapterOutput(t *testing.T) {
	tests := []struct {
		name   string
		output ports.PluginConfig
	}{
		{name: "nil", output: nil},
		{name: "typed nil", output: (*mutableIdentityTestConfig)(nil)},
		{name: "wrong kind", output: freezeOutputConfig{kind: "other"}},
		{name: "durable capability removed", output: freezeOutputConfig{kind: "identity"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := freezePluginConfig(maliciousDurableFreezer{output: tt.output})
			require.Error(t, err)
			assert.ErrorIs(t, err, shared.ErrInvalidConfig)
		})
	}
}

func TestCloneConfigForBuild_RejectsNonFreezableCredentialConfigBeforeMutation(t *testing.T) {
	pluginCfg := &nonFreezableCredentialConfig{}
	cfg := supervisorTestConfig("r1")
	cfg.Receivers[0].Config = pluginCfg

	_, err := cloneConfigForBuild(cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
	assert.False(t, pluginCfg.credentialsURICalled)
	assert.False(t, pluginCfg.applyCalled)
}

func TestSupervisor_InvalidFrozenOutputLeavesRuntimeAndConfigUntouched(t *testing.T) {
	onSwap, swaps := swapChan(1)
	factory := &countingTransportFactory{}
	s := NewSupervisor(WithOnSwap(onSwap))
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("identity", factory)

	oldCfg := configWithDurableSessionIdentity(1, "stable-state")
	changes := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, oldCfg, changes)
	defer func() { cancel(); <-errCh }()

	oldRuntime := s.Runtime()
	beforeSessions, _, _ := factory.Counts()
	proposed := configWithDurableSessionIdentity(2, "stable-state")
	proposed.Sessions[0].Config = maliciousDurableFreezer{output: nil}
	require.True(t, sendConfig(changes, proposed, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.ErrorIs(t, ev.Error, shared.ErrInvalidConfig)
	assert.Same(t, oldRuntime, s.Runtime())
	require.NotNil(t, s.Config())
	assert.Equal(t, 1, s.Config().Version)
	afterSessions, _, _ := factory.Counts()
	assert.Equal(t, beforeSessions, afterSessions)
}
