package bridge

import (
	"fmt"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// validateDurableBrokerIdentities refuses a configuration in which two
// persistent or exclusive sessions would connect as the same client to the same
// broker: they would disconnect each other on every connect. It also refuses a
// durable session whose ownership domains cannot be computed, and one whose
// config lacks the adapter-owned freeze capability, because its identity could
// change between validation and build. Only referenced sessions count, as only
// those are built. It reads the configuration alone, so it runs on every
// configuration: in every Preflight, and on the Supervisor's apply path before
// a paused reload records a document.
//
// A changed, removed or renamed broker identity is not refused: the reload
// ends the state the old identity leaves on the broker (ADR 0024).
func validateDurableBrokerIdentities(cfg *ports.BridgeConfig) error {
	if cfg == nil {
		return nil
	}
	referenced := referencedSessionIDs(cfg)
	owners := make(map[string]string)
	for _, session := range cfg.Sessions {
		mode := normalizedSessionMode(session.SessionMode)
		if !referenced[session.ID] ||
			(mode != connectivity.SessionPersistent && mode != connectivity.SessionExclusive) {
			continue
		}
		identityConfig, ok := session.Config.(ports.DurableSessionIdentityConfig)
		if !ok {
			continue
		}
		if ports.IsNilPluginConfig(session.Config) {
			return fmt.Errorf("bridge: durable session %q identity config is nil", session.ID)
		}
		if _, ok := session.Config.(ports.FreezableConfig); !ok {
			return fmt.Errorf("bridge: durable session %q identity config lacks adapter-owned freeze capability", session.ID)
		}
		domains, err := identityConfig.DurableSessionIdentityDomains(mode)
		if err != nil || len(domains) == 0 {
			return fmt.Errorf("bridge: durable session %q identity domains cannot be verified", session.ID)
		}
		for _, domain := range domains {
			if domain == "" {
				return fmt.Errorf("bridge: durable session %q identity domains cannot be verified", session.ID)
			}
			collisionKey := session.Config.Kind() + "\x00" + domain
			if owner, duplicate := owners[collisionKey]; duplicate && owner != session.ID {
				return fmt.Errorf("bridge: durable sessions %q and %q have duplicate effective broker identities", owner, session.ID)
			}
			owners[collisionKey] = session.ID
		}
	}
	return nil
}

func normalizedSessionMode(mode string) connectivity.SessionMode {
	if mode == "" {
		return connectivity.SessionEphemeral
	}
	return connectivity.SessionMode(mode)
}
