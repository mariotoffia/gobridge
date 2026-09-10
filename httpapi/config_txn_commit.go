package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// isConfigNotFound recognizes missing config across file and non-file stores.
// Only load boundaries use it; a failed save or restore remains a failure.
func isConfigNotFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, shared.ErrNotFound)
}

// Commit validates the final merged config and writes it to the config
// file using optimistic concurrency control. It reads the on-disk config,
// verifies that its version matches the version captured when the
// transaction was created (check-and-set), increments the version, and
// writes. Returns errVersionConflict if another instance committed a
// different version in the meantime. The transaction is cleaned up on
// success.
//
// Note: on network filesystems (NFS/EFS) a plain file-backed store's
// check-read and write are not perfectly atomic. When the configured
// ports.ConfigStore also implements ports.ConditionalConfigStore the commit
// uses SaveIfVersion for a genuinely atomic cross-instance CAS (e.g. the
// DynamoDB-backed config profile); otherwise it degrades to the best-effort
// read-check-Save with the documented last-writer-wins caveat.
func (m *configTxnManager) Commit(ctx context.Context, txnID string) (int, error) {
	// Serialize the entire commit pipeline (durable write + out-of-lock apply +
	// rollback). See the commitMu field comment: this prevents a concurrent
	// commit from landing a durable write or running a second runtime swap
	// inside this commit's apply window, which a blind rollback would otherwise
	// clobber. m.mu is intentionally NOT held across the apply.
	m.commitMu.Lock()
	defer m.commitMu.Unlock()

	merged, newVersion, prior, applier, err := m.commitDurable(ctx, txnID)
	if err != nil {
		return 0, err
	}

	// Apply OUTSIDE the manager lock and on a context DETACHED from the request.
	// The durable write already succeeded; the build/connect/drain that follows
	// can take tens of seconds, and holding m.mu across it would block every
	// other txn endpoint. Detaching from the request context means a client
	// disconnect or write-deadline can no longer cancel the runtime swap
	// mid-flight AFTER the durable write — the durable commit and the in-band
	// apply must not be torn apart.
	if applier != nil {
		applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitApplyTimeout)
		defer cancel()
		if applyErr := applier(applyCtx, merged); applyErr != nil {
			// ports.ErrApplyInFlight is the "committed, NOT confirmed applied, DO
			// NOT roll back" terminal signal: the runtime accepted cfg but its
			// running state is not confirmed (the swap is still in-flight past
			// the apply deadline, or the bridge is paused/shutting down and
			// recorded cfg for a later resume). In every such case cfg is (or
			// will become) the durable desired state, so rolling the durable
			// write back would fight the runtime. Keep the committed config on
			// disk and surface committed_not_applied; the file watcher / next
			// swap converges the observable state. This is the reconciliation
			// path for a crash after the durable write leaves an unapplied
			// config: the durable write is deliberately RETAINED so a restart
			// recovers the committed config instead of losing it to a rollback.
			if errors.Is(applyErr, ports.ErrApplyInFlight) {
				return newVersion, fmt.Errorf("%w: version %d is committed to disk; apply is in-flight and the running state is not confirmed (not rolled back): %w",
					errConfigApplyFailed, newVersion, applyErr)
			}
			return m.rollbackAfterApplyFailure(ctx, newVersion, prior, applyErr)
		}
	}

	return newVersion, nil
}

// rollbackAfterApplyFailure restores the previous on-disk config after a failed
// in-band apply so a process restart recovers the last good config instead of
// crash-looping on the rejected one (the "committed_not_applied restart bomb").
// It reports rolled_back on success. When there is no previous config (first
// write) or the restore write itself fails, disk still holds the rejected
// config, so it falls back to committed_not_applied and the operator reconciles.
func (m *configTxnManager) rollbackAfterApplyFailure(ctx context.Context, newVersion int, prior *ports.BridgeConfig, applyErr error) (int, error) {
	if prior == nil {
		// First write: nothing to roll back to. Disk holds the new (rejected)
		// config; report committed_not_applied honestly.
		return newVersion, fmt.Errorf("%w: version %d is on disk but the running runtime did not converge: %w",
			errConfigApplyFailed, newVersion, applyErr)
	}

	// Restore on a FRESH context: the apply context may already be past its
	// deadline (a timed-out apply), which would fail the restore write. The
	// restore mirrors the commit's write discipline (CAS when the store
	// supports it) so the rollback cannot clobber a concurrent commit.
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitApplyTimeout)
	defer cancel()
	if restoreErr := m.restoreConfig(restoreCtx, prior, newVersion); restoreErr != nil {
		if m.logger != nil {
			m.logger.Error("config commit: apply failed AND rollback did not restore the previous config (write failed or a concurrent commit advanced the version); disk and runtime may diverge",
				"failed_version", newVersion, "restore_error", restoreErr, "apply_error", applyErr)
		}
		return newVersion, fmt.Errorf("%w: version %d is on disk but the running runtime did not converge, and rollback did not restore the previous config (%v): %w",
			errConfigApplyFailed, newVersion, restoreErr, applyErr)
	}

	if m.logger != nil {
		m.logger.Warn("config commit: apply failed; on-disk config rolled back to previous version",
			"rejected_version", newVersion, "restored_version", prior.Version, "apply_error", applyErr)
	}
	return prior.Version, fmt.Errorf("%w: apply of version %d failed; disk restored to version %d: %w",
		errConfigRolledBack, newVersion, prior.Version, applyErr)
}

// restoreConfig writes prior back to the store during a rollback, mirroring
// commitDurable's write discipline. Against a ports.ConditionalConfigStore it
// uses SaveIfVersion(prior, committedVersion) so the rollback only lands while
// THIS transaction's just-committed version is still the latest on disk; if a
// concurrent writer already advanced past it, the CAS refuses rather than
// clobbering that acknowledged commit — closing the same lost-update window on
// the rollback path that the forward commit closes. Stores without the
// capability keep the plain Save (documented single-writer last-writer-wins).
func (m *configTxnManager) restoreConfig(ctx context.Context, prior *ports.BridgeConfig, committedVersion int) error {
	if cas, ok := m.store.(ports.ConditionalConfigStore); ok {
		return cas.SaveIfVersion(ctx, prior, committedVersion)
	}
	return m.store.Save(ctx, prior)
}

// commitDurable performs the transactional, DURABLE portion of a commit under
// the manager lock: compute+validate the merged config, guard against plugin-
// option loss, CAS the on-disk version, Save, then clear the transaction and
// stop its TTL timer. It returns the merged config, the new version, the PRIOR
// on-disk config (for rollback if the post-lock apply fails; nil on first
// write), and the applier to run AFTER the lock is released (nil when none is
// wired). The lock is intentionally NOT held across the apply — see Commit.
func (m *configTxnManager) commitDurable(ctx context.Context, txnID string) (*ports.BridgeConfig, int, *ports.BridgeConfig, func(context.Context, *ports.BridgeConfig) error, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkTxn(txnID); err != nil {
		return nil, 0, nil, nil, err
	}

	merged, err := m.computeMerged(ctx)
	if err != nil {
		return nil, 0, nil, nil, err
	}

	if _, valErr := m.store.Validate(ctx, merged); valErr != nil {
		return nil, 0, nil, nil, valErr
	}

	// Guard the CRITICAL config-corruption class: a PATCH that touches an
	// existing plugin-config-bearing entry must never drop that entry's typed
	// Config (its broker URL/credentials/options live only in the typed
	// Config; the disk projection writes options from Config alone, so a nil
	// Config erases them permanently). If any entry that HAD a non-nil Config
	// in the running config lost it in the merge, refuse to persist. This is
	// belt-and-suspenders behind the merge layer (which preserves Config on
	// scalar PATCHes) and also correctly rejects transport changes attempted
	// via PATCH, which cannot carry replacement options.
	if err := guardNoConfigLoss(m.configProvider(), merged); err != nil {
		return nil, 0, nil, nil, err
	}

	// CAS: read the current on-disk config once. Its version drives the
	// check-and-set, and the config itself is retained as the rollback target
	// if the post-lock apply fails. A missing config, from either a file or
	// another source, is first-write: version 0 and no prior config to restore.
	prior, err := m.store.Load(ctx)
	var diskVersion int
	switch {
	case err == nil:
		diskVersion = prior.Version
	case isConfigNotFound(err):
		prior = nil
		diskVersion = 0
	default:
		return nil, 0, nil, nil, fmt.Errorf("config commit: read disk version: %w", err)
	}
	if diskVersion != m.active.baseVersion {
		return nil, 0, nil, nil, fmt.Errorf("%w: expected version %d but file has version %d; re-read the config and retry",
			errVersionConflict, m.active.baseVersion, diskVersion)
	}

	newVersion := diskVersion + 1
	merged.Version = newVersion

	// Persist with an ATOMIC compare-and-swap when the store supports it. A
	// plain read-check-Save (the version guard above followed by Save) has a
	// lost-update window between the version read and the write: two admin
	// instances both committing from version N against a shared backend each
	// pass the guard, and the second Save clobbers the first — last-writer-wins,
	// a silently dropped acknowledged commit (and a version regression should a
	// later rollback fire). ports.ConditionalConfigStore.SaveIfVersion closes
	// that window: it writes only if the store's CURRENTLY persisted version
	// still equals the transaction's baseVersion, so a concurrent commit that
	// advanced the version is rejected with shared.ErrVersionMismatch (surfaced
	// as errVersionConflict) instead of being overwritten. Stores that do NOT
	// implement the capability have no safe cross-instance write, so the plain
	// Save is taken ONLY when the operator asserted a single writer
	// (m.singleWriter); otherwise the commit fails closed with
	// errConfigStoreNotCAS rather than performing a silent last-writer-wins Save.
	if cas, ok := m.store.(ports.ConditionalConfigStore); ok {
		if err := cas.SaveIfVersion(ctx, merged, m.active.baseVersion); err != nil {
			if errors.Is(err, shared.ErrVersionMismatch) {
				return nil, 0, nil, nil, fmt.Errorf("%w: expected version %d but a concurrent commit advanced the shared config; re-read the config and retry",
					errVersionConflict, m.active.baseVersion)
			}
			return nil, 0, nil, nil, fmt.Errorf("config write failed: %w", err)
		}
	} else if !m.singleWriter {
		// Fail closed: a non-CAS store on a possibly-multi-writer deployment
		// cannot serialize concurrent commits. The read-time version guard above
		// is NOT atomic with this write, so two admin instances that both read
		// version N would each pass it and the second plain Save would clobber
		// the first acknowledged commit (silent lost update;). Refuse
		// the durable write instead of performing it silently. The operator must
		// either wire a ports.ConditionalConfigStore (always safe) or assert a
		// single writer via Config.ConfigSingleWriter.
		return nil, 0, nil, nil, fmt.Errorf("%w: the configured config store does not support compare-and-swap saves; enable it with a ports.ConditionalConfigStore or set Config.ConfigSingleWriter to assert a single admin writer",
			errConfigStoreNotCAS)
	} else if err := m.store.Save(ctx, merged); err != nil {
		// Single-writer asserted: a plain Save is safe because no peer can
		// clobber this write. This is the documented single-writer LWW path.
		return nil, 0, nil, nil, fmt.Errorf("config write failed: %w", err)
	}

	merged = m.projectionOf(ctx, merged)

	// The durable write and version transition are complete, so the transaction
	// is logically done regardless of the apply outcome. Clear it (and stop the
	// TTL timer) NOW, before releasing the lock, so a long apply neither blocks
	// other txn endpoints nor races the expiry timer into a spurious rollback.
	applier := m.applier
	m.cleanupLocked()
	return merged, newVersion, prior, applier, nil
}
