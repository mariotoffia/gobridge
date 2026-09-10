package config

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func TestInitializeOnlyAbsentDocuments(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *ports.BridgeConfig
		err  error
	}{
		{"present", &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "empty-valid"}}, nil},
		{"invalid", &ports.BridgeConfig{}, nil},
		{"denied", nil, shared.ErrNotAuthorized},
		{"invalid with nested not found", nil, shared.ErrInvalidConfig.Wrap(shared.ErrNotFound)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceFault := errors.New("source must not be accessed")
			target := &initialStore{cfg: tc.cfg, loadErr: tc.err}
			err := Initialize(t.Context(), target, &stubLoader{err: sourceFault}, nil)
			require.NotErrorIs(t, err, sourceFault)
			if tc.name == "present" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Nil(t, target.persisted)
		})
	}
	require.ErrorIs(t, Initialize(t.Context(), &initialStore{}, nil, nil), shared.ErrNotFound)
}

func TestInitializeFreezesBeforeAdmissionAndOwnsVersion(t *testing.T) {
	seed := minimalValidConfig("seed")
	seed.Version = 99
	seed.Receivers[0].Config = &initialPlugin{Values: map[string]string{"credential": "reference"}}
	target := &initialStore{}
	err := Initialize(t.Context(), target, &stubLoader{cfg: seed}, func(_ context.Context, cfg *ports.BridgeConfig) error {
		cfg.Receivers[0].Config.(*initialPlugin).Values["credential"] = "resolved"
		cfg.Routes[0].Bindings[0] = "mutated"
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 99, seed.Version)
	require.Equal(t, 1, target.persisted.Version)
	require.Equal(t, "reference", target.persisted.Receivers[0].Config.(*initialPlugin).Values["credential"])
	require.Equal(t, "bind1", target.persisted.Routes[0].Bindings[0])
}

func TestInitializeAdmissionCannotMutateClusterMembers(t *testing.T) {
	seed := minimalValidConfig("seed")
	seed.Bridge.Cluster = &ports.ClusterConfig{Members: []string{"original"}}
	target := &initialStore{}
	err := Initialize(t.Context(), target, &stubLoader{cfg: seed}, func(_ context.Context, cfg *ports.BridgeConfig) error {
		cfg.Bridge.Cluster.Members[0] = "changed-by-admission"
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"original"}, seed.Bridge.Cluster.Members, "admission must not mutate the source")
	require.Equal(t, []string{"original"}, target.persisted.Bridge.Cluster.Members, "admission must not mutate the committed candidate")
	require.NotSame(t, &seed.Bridge.Cluster.Members[0], &target.persisted.Bridge.Cluster.Members[0])
}

func TestInitializeAdmissionCannotMutateBackoffJitter(t *testing.T) {
	seed := minimalValidConfig("seed")
	jitter := 0.25
	seed.Routes[0].Policy.Backoff.Jitter = &jitter
	target := &initialStore{}
	err := Initialize(t.Context(), target, &stubLoader{cfg: seed}, func(_ context.Context, cfg *ports.BridgeConfig) error {
		*cfg.Routes[0].Policy.Backoff.Jitter = 0.75
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 0.25, *seed.Routes[0].Policy.Backoff.Jitter, "admission must not mutate the source")
	require.Equal(t, 0.25, *target.persisted.Routes[0].Policy.Backoff.Jitter, "admission must not mutate the committed candidate")
	require.NotSame(t, seed.Routes[0].Policy.Backoff.Jitter, target.persisted.Routes[0].Policy.Backoff.Jitter)
}

func TestInitializeReloadsDifferentWinnerAndPreservesUnrelatedErrors(t *testing.T) {
	for _, fault := range []error{nil, shared.ErrTimeout, shared.ErrNotAuthorized} {
		target := &initialStore{winner: minimalValidConfig("winner"), writeErr: fault}
		target.winner.Version = 7
		var admitted []string
		err := Initialize(t.Context(), target, &stubLoader{cfg: minimalValidConfig("loser")}, func(_ context.Context, cfg *ports.BridgeConfig) error {
			admitted = append(admitted, cfg.Bridge.ID)
			return nil
		})
		if errors.Is(fault, shared.ErrNotAuthorized) {
			require.ErrorIs(t, err, fault)
		} else {
			require.NoError(t, err)
		}
		require.Equal(t, []string{"loser", "winner"}, admitted)
		require.Equal(t, 7, target.cfg.Version)
		require.Nil(t, target.persisted)
	}
}

func TestInitializeRejectsUnsupportedAndInvalidInputs(t *testing.T) {
	target := &initialStore{}
	readWriteOnly := struct{ ports.ConfigStore }{target}
	require.ErrorIs(t, Initialize(t.Context(), readWriteOnly, &stubLoader{}, nil), shared.ErrNotSupported)
	require.ErrorIs(t, Initialize(t.Context(), target, &stubLoader{}, nil), shared.ErrInvalidConfig)
	require.ErrorIs(t, Initialize(t.Context(), nil, nil, nil), shared.ErrInvalidConfig)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, Initialize(ctx, target, nil, nil), context.Canceled)
	sourceFault := errors.New("source unreadable")
	require.ErrorIs(t, Initialize(t.Context(), target, &stubLoader{err: sourceFault}, nil), sourceFault)
	admissionFault := errors.New("admission refused")
	require.ErrorIs(t, Initialize(t.Context(), target, &stubLoader{cfg: minimalValidConfig("refused")},
		func(context.Context, *ports.BridgeConfig) error { return admissionFault }), admissionFault)
	require.Nil(t, target.persisted)
}
