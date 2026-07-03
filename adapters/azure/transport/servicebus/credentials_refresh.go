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
// from the new material and atomically swapped in under the stack
// lock; the poll loop's next snapshot (currentClient) picks it up.
// The live client is NEVER nilled — if the rebuild fails, the old
// stack stays in place (its credentials may keep working until
// expiry) and the error is returned for the supervisor to retry.
//
// In-flight deliveries keep settling against the link they were
// received on (rawInbound pins the client); once the old link closes,
// their unsettled locks lapse and the broker redelivers — normal
// at-least-once semantics.
//
// Ordering: for non-session receivers the new stack is built FIRST and
// the old one closed after the swap. A session receiver is the
// opposite corner: the new accept cannot succeed while the old link
// still holds the session lock, so the old stack is closed first and
// the accept (with its rolling-deploy retry) runs after; during that
// gap the poll loop sees transient receive errors on the closed link
// and backs off — degraded but never nil, never a panic.
func (r *Receiver) ApplyCredentials(ctx context.Context, set *connectivity.CredentialSet) error {
	if set == nil {
		return shared.ErrInvalidPayload.WithMessage("servicebus: nil credential set")
	}

	r.initMu.Lock()
	newConn, changed := credentialsToConnection(r.cfg.Connection, set)
	if !changed {
		r.initMu.Unlock()
		return nil
	}
	r.cfg.Connection = newConn
	injected := r.cfg.Client != nil
	started := r.client != nil
	sessionMode := r.cfg.SessionID != ""
	r.initMu.Unlock()

	if injected || !started {
		// Injected test client: nothing to rebuild. Not started yet:
		// ensureClient builds from the updated Connection on Run.
		return nil
	}

	if sessionMode {
		old := r.swapStack(receiverStack{})
		old.close(ctx)
	}

	stack, err := r.buildStack(ctx, newConn)
	if err != nil {
		// Non-session: old stack still installed and polling. Session:
		// the poll loop errors with backoff until a later rotation (or
		// restart) succeeds — visible, recoverable, no nil panic.
		return err
	}

	old := r.swapStack(stack)
	old.close(ctx)

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug,
			"servicebus: receiver stack rotated",
			"entity", r.entityName(),
			"session_id", r.cfg.SessionID,
		)
	}
	return nil
}
