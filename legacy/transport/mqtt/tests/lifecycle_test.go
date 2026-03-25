// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Lifecycle Transaction Tests
//
// Tests for LifecycleCoordinator and LifecycleTransaction implementations.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ L001 │ BeginTransaction returns transaction   │ PASS     │
// │ L002 │ AddSource schedules addition           │ PASS     │
// │ L003 │ RemoveSource schedules removal         │ PASS     │
// │ L004 │ UpdateSource schedules update          │ PASS     │
// │ L005 │ AddTarget schedules addition           │ PASS     │
// │ L006 │ RemoveTarget schedules removal         │ PASS     │
// │ L007 │ UpdateTarget schedules update          │ PASS     │
// │ L008 │ Commit after Commit returns error      │ PASS     │
// │ L009 │ Commit after Rollback returns error    │ PASS     │
// │ L010 │ Rollback after Commit returns error    │ PASS     │
// │ L011 │ Rollback releases lock                 │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package mqtttests

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/transport/mqtt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// BeginTransaction Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestBeginTransaction validates transaction creation.
func TestBeginTransaction(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	require.NotNil(t, coordinator)

	ctx := context.Background()
	tx, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)
	require.NotNil(t, tx)

	// Must rollback to release lock
	err = tx.Rollback()
	assert.NoError(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// Source Operations Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTransaction_AddSource validates source addition scheduling.
func TestTransaction_AddSource(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	ctx := context.Background()
	tx, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	// Schedule source addition
	srcCfg := &mqtt.SourceConfigImpl{
		ID:     "new-source",
		Topics: []string{"test/topic"},
	}
	err = tx.AddSource(srcCfg)
	assert.NoError(t, err)
}

// TestTransaction_RemoveSource validates source removal scheduling.
func TestTransaction_RemoveSource(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	ctx := context.Background()
	tx, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	// Schedule source removal
	err = tx.RemoveSource("existing-source")
	assert.NoError(t, err)
}

// TestTransaction_UpdateSource validates source update scheduling.
func TestTransaction_UpdateSource(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	ctx := context.Background()
	tx, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	// Schedule source update
	newCfg := &mqtt.SourceConfigImpl{
		ID:     "existing-source",
		Topics: []string{"new/topic"},
		QoS:    2,
	}
	err = tx.UpdateSource("existing-source", newCfg)
	assert.NoError(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// Target Operations Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTransaction_AddTarget validates target addition scheduling.
func TestTransaction_AddTarget(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	ctx := context.Background()
	tx, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	// Schedule target addition
	tgtCfg := &mqtt.TargetConfigImpl{
		ID:           "new-target",
		DefaultTopic: "test/topic",
	}
	err = tx.AddTarget(tgtCfg)
	assert.NoError(t, err)
}

// TestTransaction_RemoveTarget validates target removal scheduling.
func TestTransaction_RemoveTarget(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	ctx := context.Background()
	tx, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	// Schedule target removal
	err = tx.RemoveTarget("existing-target")
	assert.NoError(t, err)
}

// TestTransaction_UpdateTarget validates target update scheduling.
func TestTransaction_UpdateTarget(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	ctx := context.Background()
	tx, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	// Schedule target update
	newCfg := &mqtt.TargetConfigImpl{
		ID:           "existing-target",
		DefaultTopic: "new/topic",
		QoS:          2,
	}
	err = tx.UpdateTarget("existing-target", newCfg)
	assert.NoError(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// Transaction State Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestTransaction_CommitAfterCommit validates double commit returns error.
func TestTransaction_CommitAfterCommit(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	ctx := context.Background()
	tx, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)

	// First commit (may fail because connection not started, but that's OK)
	_, _ = tx.Commit(ctx)

	// Second commit should return error
	_, err = tx.Commit(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already completed")
}

// TestTransaction_CommitAfterRollback validates commit after rollback returns error.
func TestTransaction_CommitAfterRollback(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	ctx := context.Background()
	tx, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)

	// Rollback first
	err = tx.Rollback()
	require.NoError(t, err)

	// Commit after rollback should return error
	_, err = tx.Commit(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already completed")
}

// TestTransaction_RollbackAfterCommit validates rollback after commit returns error.
func TestTransaction_RollbackAfterCommit(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	ctx := context.Background()
	tx, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)

	// Commit first (may fail because connection not started)
	_, _ = tx.Commit(ctx)

	// Rollback after commit should return error
	err = tx.Rollback()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already completed")
}

// TestTransaction_RollbackReleasesLock validates rollback allows new transaction.
func TestTransaction_RollbackReleasesLock(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	ctx := context.Background()

	// First transaction
	tx1, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)

	// Rollback releases the lock
	err = tx1.Rollback()
	require.NoError(t, err)

	// Should be able to start a new transaction
	tx2, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)
	defer tx2.Rollback()

	assert.NotNil(t, tx2, "should be able to create new transaction after rollback")
}

// TestTransaction_AddAfterComplete validates operations fail after completion.
func TestTransaction_AddAfterComplete(t *testing.T) {
	cfg := &mqtt.MQTTConnectionConfig{
		ID: "test-conn",
		Connection: mqtt.ConnectionConfig{
			BrokerURL: "tcp://localhost:1883",
		},
	}
	conn, err := mqtt.NewConnection(cfg)
	require.NoError(t, err)

	coordinator := conn.LifecycleCoordinator()
	ctx := context.Background()
	tx, err := coordinator.BeginTransaction(ctx)
	require.NoError(t, err)

	// Rollback to complete the transaction
	err = tx.Rollback()
	require.NoError(t, err)

	// AddSource should fail
	srcCfg := &mqtt.SourceConfigImpl{ID: "test", Topics: []string{"topic"}}
	err = tx.AddSource(srcCfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already completed")

	// RemoveSource should fail
	err = tx.RemoveSource("test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already completed")

	// AddTarget should fail
	tgtCfg := &mqtt.TargetConfigImpl{ID: "test", DefaultTopic: "topic"}
	err = tx.AddTarget(tgtCfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already completed")
}
