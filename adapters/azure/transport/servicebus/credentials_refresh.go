package servicebus

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// credentialsToConnection translates a domain.CredentialSet into the
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
func credentialsToConnection(existing ConnectionConfig, set *domain.CredentialSet) (ConnectionConfig, bool) {
	if set == nil {
		return existing, false
	}
	out := existing
	changed := false

	if set.Password != nil {
		if set.Password.Username == "" {
			// Opaque connection string path.
			if out.ConnectionString != set.Password.Password {
				out.ConnectionString = set.Password.Password
				out.ClientID = ""
				out.ClientSecret = ""
				changed = true
			}
		} else {
			if out.ClientID != set.Password.Username ||
				out.ClientSecret != set.Password.Password {
				out.ClientID = set.Password.Username
				out.ClientSecret = set.Password.Password
				out.ConnectionString = ""
				changed = true
			}
		}
	}

	if set.TLS != nil {
		newCA := joinASBPEMs(set.TLS.CAPEMs)
		if out.CaPEM != newCA ||
			out.ClientCertPEM != set.TLS.CertPEM ||
			out.ClientKeyPEM != set.TLS.KeyPEM ||
			out.InsecureSkipVerify != set.TLS.InsecureSkipVerify {
			out.CaPEM = newCA
			out.ClientCertPEM = set.TLS.CertPEM
			out.ClientKeyPEM = set.TLS.KeyPEM
			out.InsecureSkipVerify = set.TLS.InsecureSkipVerify
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
func (s *Sender) ApplyCredentials(ctx context.Context, set *domain.CredentialSet) error {
	if set == nil {
		return shared.ErrInvalidPayload.WithMessage("servicebus: nil credential set")
	}
	newConn, changed := credentialsToConnection(s.cfg.Connection, set)
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

	s.initMu.Lock()
	oldClient := s.asbClient
	oldSender := s.client
	s.client = newSender
	s.asbClient = asbClient
	s.cfg.Connection = newConn
	s.initMu.Unlock()

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
// Same contract as Sender.ApplyCredentials. The receiver's active
// message pump is not interrupted; the next Run after Close picks up
// the rotated material.
//
// Implementation detail: we do not tear down the active asbAPI link
// here because the Receiver's Run loop polls on it and will emit
// bogus errors mid-flight. Instead we stash the new config; the
// long-running Run is expected to be restarted by the framework
// (Close → Run) on credential rotation. This is conservative but
// matches how most supervisors already handle config changes.
func (r *Receiver) ApplyCredentials(_ context.Context, set *domain.CredentialSet) error {
	if set == nil {
		return shared.ErrInvalidPayload.WithMessage("servicebus: nil credential set")
	}
	newConn, changed := credentialsToConnection(r.cfg.Connection, set)
	if !changed {
		return nil
	}

	r.initMu.Lock()
	r.cfg.Connection = newConn
	// Clearing client forces ensureClient to rebuild on the next Run
	// invocation. Active Run goroutines keep using the cached local
	// client reference until they exit.
	r.client = nil
	r.scheduler = nil
	r.initMu.Unlock()
	return nil
}
