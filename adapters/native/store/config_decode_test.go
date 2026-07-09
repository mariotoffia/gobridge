package nativestore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

// These tests exercise the FULL production config seam for the in-memory
// store: Register installs decodeMemoryConfig on a real ports.Registry, and
// Registry.Decode runs the operator's `options:` block through the production
// RawConfig decoder (parser.NewRawConfig — TagName "json", ErrorUnused) exactly
// as the runtime does. The regression guarded here (c10-memlease-factory-wiring)
// is that decodeMemoryConfig once ignored its RawConfig and dropped the
// acknowledge_single_replica flag, leaving NewLeaseStore's fail-closed gate
// unsatisfiable from any parsed config — the factory told the operator to set a
// flag that no config path could deliver. Importing config/parser from an
// adapter test is allowed: go-arch-lint excludes *_test.go, and there is
// precedent (amqp091, azure, native/config/file).

func mustRegistry(t *testing.T) *ports.Registry {
	t.Helper()

	reg := ports.NewRegistry()
	require.NoError(t, Register(reg))

	return reg
}

// TestDecodeMemoryConfig_CarriesAcknowledgeFlag proves the flag survives the
// registry decode path. Mutation: revert decodeMemoryConfig to
// `return MemoryConfig{}, nil` and this FAILs (the flag is observed false).
func TestDecodeMemoryConfig_CarriesAcknowledgeFlag(t *testing.T) {
	reg := mustRegistry(t)

	pc, err := reg.Decode(MemoryKind, parser.NewRawConfig(map[string]any{
		"acknowledge_single_replica": true,
	}))
	require.NoError(t, err)
	require.True(t, memoryConfigFrom(pc).AcknowledgeSingleReplica,
		"acknowledge_single_replica must round-trip through the production decoder")
}

// TestDecodeMemoryConfig_EmptyOptionsInert proves the outbox/DLQ path — a
// memory store configured with no options — still decodes to the inert zero
// value (flag false), so tagging the field never forces the flag onto the
// non-lease memory stores that share MemoryConfig.
func TestDecodeMemoryConfig_EmptyOptionsInert(t *testing.T) {
	reg := mustRegistry(t)

	cases := map[string]ports.RawConfig{
		"nil-raw":       nil,
		"empty-options": parser.NewRawConfig(map[string]any{}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			pc, err := reg.Decode(MemoryKind, raw)
			require.NoError(t, err)
			require.False(t, memoryConfigFrom(pc).AcknowledgeSingleReplica)
		})
	}
}

// TestDecodeMemoryConfig_UnknownKeyRejected proves the strict ErrorUnused
// contract is intact: an undocumented option key fails the whole decode rather
// than being silently dropped.
func TestDecodeMemoryConfig_UnknownKeyRejected(t *testing.T) {
	reg := mustRegistry(t)

	_, err := reg.Decode(MemoryKind, parser.NewRawConfig(map[string]any{
		"acknowledge_single_replica": true,
		"bogus_key":                  "nope",
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogus_key")
}

// TestMemoryLease_DecodeToBuildSeam is the end-to-end proof the review demanded:
// the operator's parsed config must be able to UNLOCK the fail-closed gate that
// the factory's own error message names. It runs config → Registry.Decode →
// NewLeaseStore, asserting the flag both unlocks construction and, when absent,
// fails closed with the documented remedy.
func TestMemoryLease_DecodeToBuildSeam(t *testing.T) {
	factory := NewMemoryStoreFactory()

	t.Run("flag set unlocks construction", func(t *testing.T) {
		reg := mustRegistry(t)

		pc, err := reg.Decode(MemoryKind, parser.NewRawConfig(map[string]any{
			"acknowledge_single_replica": true,
		}))
		require.NoError(t, err)

		store, err := factory.NewLeaseStore(context.Background(), pc)
		require.NoError(t, err)
		require.NotNil(t, store)
	})

	t.Run("flag absent fails closed", func(t *testing.T) {
		reg := mustRegistry(t)

		pc, err := reg.Decode(MemoryKind, parser.NewRawConfig(map[string]any{}))
		require.NoError(t, err)

		store, err := factory.NewLeaseStore(context.Background(), pc)
		require.Error(t, err)
		require.Nil(t, store)
		require.Contains(t, err.Error(), "acknowledge_single_replica")
	})
}
