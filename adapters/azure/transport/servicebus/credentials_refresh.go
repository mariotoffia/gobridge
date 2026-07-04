package servicebus

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// credentialsToConnection translates a connectivity.CredentialSet into the
// subset of ConnectionConfig that can be rotated. The convention is:
//
//   - PasswordCredential.Username empty, Password holds the full SAS
//     connection string. This matches how ASB typically publishes
//     rotated SAS credentials (one opaque string).
//   - Username and Password together become ClientID + ClientSecret
//     when the session was configured for AAD auth. The caller must
//     have set cfg.TenantID already; we don't attempt to derive it.
//   - TLSMaterial maps onto CaPEM / ClientCertPEM / ClientKeyPEM.
//     When any of those fields would change, the caller MUST rebuild
//     the *azservicebus.Client; setting TLSConfig to nil here
//     signals buildClientOptions to rebuild tls.Config from the PEM
//     fields.
//
// Returning changed=false signals "nothing rotatable in this set".
func credentialsToConnection(existing ConnectionConfig, set *connectivity.CredentialSet) (ConnectionConfig, bool) {
	if set == nil {
		return existing, false
	}
	out := existing
	changed := false

	if set.Password() != nil {
		if set.Password().Username() == "" {
			// Opaque connection string path.
			if !out.ConnectionString.Equal(set.Password().Password()) {
				out.ConnectionString = set.Password().Password()
				out.ClientID = ""
				out.ClientSecret = shared.Secret{}
				changed = true
			}
		} else {
			if out.ClientID != set.Password().Username() ||
				!out.ClientSecret.Equal(set.Password().Password()) {
				out.ClientID = set.Password().Username()
				out.ClientSecret = set.Password().Password()
				out.ConnectionString = shared.Secret{}
				changed = true
			}
		}
	}

	if set.TLS() != nil {
		newCA := joinASBPEMs(set.TLS().CAPEMs())
		if out.CaPEM.Reveal() != newCA ||
			out.ClientCertPEM.Reveal() != set.TLS().CertPEM() ||
			!out.ClientKeyPEM.Equal(set.TLS().KeyPEM()) ||
			out.InsecureSkipVerify != set.TLS().InsecureSkipVerify() {
			out.CaPEM = shared.NewSecret(newCA)
			out.ClientCertPEM = shared.NewSecret(set.TLS().CertPEM())
			out.ClientKeyPEM = set.TLS().KeyPEM()
			out.InsecureSkipVerify = set.TLS().InsecureSkipVerify()
			// Force buildClientOptions to rebuild tls.Config from PEMs.
			out.TLSConfig = nil
			changed = true
		}
	}

	return out, changed
}

func joinASBPEMs(pems []string) string {
	switch len(pems) {
	case 0:
		return ""
	case 1:
		return pems[0]
	}
	total := 0
	for _, p := range pems {
		total += len(p) + 1
	}
	buf := make([]byte, 0, total)
	for i, p := range pems {
		if i > 0 {
			buf = append(buf, '\n')
		}
		buf = append(buf, p...)
	}
	return string(buf)
}

// ApplyCredentials rotates the Sender's Service Bus credentials by
// rebuilding the underlying *azservicebus.Client with the new
// material and swapping the sender link. Any in-flight send finishes
// against the old link; subsequent sends use the new one.
//
// TLSMaterial on CredentialSet is ignored: azservicebus respects the
// tls.Config baked into ClientOptions at construction time, and
// swapping requires a new client — which is exactly what rotating the
// SAS/secret path does here. A dedicated TLS-only rotation would map
// cert/key PEMs into a tls.Config and rebuild; out of scope for the
// MVP.
func (s *Sender) ApplyCredentials(ctx context.Context, set *connectivity.CredentialSet) error {
	if set == nil {
		return shared.ErrInvalidPayload.WithMessage("servicebus: nil credential set")
	}
	newConn, changed := credentialsToConnection(s.connectionSnapshot(), set)
	if !changed {
		return nil
	}

	asbClient, err := rawNewAzClient(newConn)
	if err != nil {
		return shared.ErrTemporaryAuthFailure.Wrap(err)
	}

	newSender, err := asbClient.NewSender(s.entityName())
	if err != nil {
		_ = asbClient.Close(ctx)
		return shared.ErrTemporaryAuthFailure.Wrap(
			fmt.Errorf("servicebus: rotate sender for %q: %w", s.entityName(), err))
	}

	oldSender, oldClient := s.swapClient(newSender, asbClient, newConn)

	// Close the old sender and client outside the lock. Errors here
	// are logged but not returned: the rotation itself succeeded.
	if oldSender != nil {
		if err := oldSender.Close(ctx); err != nil && logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug,
				"servicebus: closing old sender after credential rotation",
				"error", err)
		}
	}
	if oldClient != nil {
		if err := oldClient.Close(ctx); err != nil && logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug,
				"servicebus: closing old client after credential rotation",
				"error", err)
		}
	}
	return nil
}

// ApplyCredentials rotates the Receiver's Service Bus credentials.
// Same contract as Sender.ApplyCredentials: a complete replacement
// stack (client handle + receiver seam + retry scheduler) is built
// from the new material and swapped in, and cfg.Connection is advanced
// ONLY after a successful build (commitStack). The live client is
// NEVER nilled on a build failure of a non-session receiver — the old
// stack stays in place (its credentials may keep working until expiry)
// and a re-push of the same credentials retries the build because
// cfg.Connection was never mutated.
//
// In-flight deliveries keep settling against the link they were
// received on (rawInbound pins the client); once the old link closes,
// their unsettled locks lapse and the broker redelivers — normal
// at-least-once semantics.
//
// Ordering differs by mode:
//
//   - Non-session: build the new stack FIRST, then commit-and-swap on
//     success and close the old stack. If the build fails the old stack
//     keeps polling and cfg.Connection is unchanged — a degraded-but-
//     alive receiver that a retry (same credentials) can heal.
//
//   - Session (pinned session_id): the new accept cannot win the
//     exclusive session lock while the old link still holds it, so the
//     old stack is closed FIRST (beginSessionRebuild) and the new
//     accept — with its rolling-deploy retry — runs after.
//     cfg.Connection stays UNCOMMITTED and the receiver is left in a
//     rebuild-pending state until the build succeeds: re-pushing the
//     SAME credentials is not short-circuited (cfg.Connection never
//     advanced), and the poll loop (rebuildPendingStack) retries the
//     build with the pending connection, so a build failure during
//     rotation self-heals instead of bricking the receiver until
//     restart.
//
//   - use_sessions: follows the NON-session ordering. The rotated build
//     produces a handle with a nil receiver seam (buildStack accepts no
//     session in this mode), commitStack swaps it in and closes the old
//     stack including the currently held session, and the poll loop
//     accepts the next available session on the new handle
//     (ensureSessionSeam). No exclusive lock is contested at the handle
//     level, so close-before-build is unnecessary.
func (r *Receiver) ApplyCredentials(ctx context.Context, set *connectivity.CredentialSet) error {
	if set == nil {
		return shared.ErrInvalidPayload.WithMessage("servicebus: nil credential set")
	}

	r.initMu.Lock()
	// When a session rebuild is already pending, compute the new
	// connection against the (uncommitted) pending target so an
	// identical re-push is recognised as "no further change" yet still
	// drives the outstanding rebuild rather than short-circuiting.
	base := r.cfg.Connection
	if r.rebuildPending {
		base = r.pendingConn
	}
	newConn, changed := credentialsToConnection(base, set)
	injected := r.cfg.Client != nil
	// A use_sessions receiver is live with a nil client seam between
	// sessions (ensureSessionSeam accepts lazily), so the handle — not
	// only the seam — marks the receiver as started.
	started := r.client != nil || r.asbClient != nil || r.rebuildPending
	sessionMode := r.cfg.SessionID != ""
	pending := r.rebuildPending
	r.initMu.Unlock()

	if !changed && !pending {
		return nil
	}

	if injected || !started {
		// Injected test client: nothing to rebuild (Connection is
		// ignored). Not started yet: stash the new connection so the
		// cold build on Run uses it. cfg.Connection is safe to commit
		// here because there is no live stack to invalidate.
		r.initMu.Lock()
		r.cfg.Connection = newConn
		r.initMu.Unlock()
		return nil
	}

	if sessionMode {
		// Exclusive session semantics: close the old link before the
		// rebuild, recording the target as pending WITHOUT committing
		// cfg.Connection. beginSessionRebuild returns the generation so
		// the commit can be fenced against a newer rotation.
		old, gen := r.beginSessionRebuild(newConn)
		old.close(ctx)

		stack, err := r.build(ctx, newConn)
		if err != nil {
			// cfg.Connection stays uncommitted and rebuildPending stays
			// set: the poll loop (or a re-push of the same credentials)
			// retries with pendingConn — visible, recoverable, never a
			// nil-panic, never short-circuited.
			return fmt.Errorf("servicebus: rebuild session receiver stack for %q: %w", r.entityName(), err)
		}

		toClose, applied := r.commitRebuild(gen, stack, newConn)
		toClose.close(ctx)
		if !applied {
			// A newer rotation superseded this build while it ran; the
			// freshly built stack was closed above and the newer stack is
			// live. Report success — the rotation intent was satisfied by
			// the newer connection.
			return nil
		}

		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug,
				"servicebus: receiver session stack rotated",
				"entity", r.entityName(),
				"session_id", r.cfg.SessionID,
			)
		}
		return nil
	}

	// Non-session: build FIRST, commit-and-swap only on success so a
	// failed build leaves the old stack polling and cfg.Connection
	// unchanged.
	stack, err := r.build(ctx, newConn)
	if err != nil {
		return fmt.Errorf("servicebus: rebuild receiver stack for %q: %w", r.entityName(), err)
	}

	old := r.commitStack(stack, newConn)
	old.close(ctx)

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug,
			"servicebus: receiver stack rotated",
			"entity", r.entityName(),
		)
	}
	return nil
}
