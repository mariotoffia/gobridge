package bridge

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/ports"
)

func (b *Builder) resolveProcessors(names []string) ([]ports.Processor, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]ports.Processor, 0, len(names))
	for _, n := range names {
		p, ok := b.processors[n]
		if !ok {
			return nil, fmt.Errorf("bridge: unknown processor %q", n)
		}
		out = append(out, p)
	}
	return out, nil
}

// resolveConfigCredentials inspects cfg for the optional
// CredentialedConfig contract, resolves the URI through the configured
// credential store, and applies the resolved material in place. It
// returns the URI (so callers can register credential-refresh
// watchers) and any error from store lookup or apply.
func (b *Builder) resolveConfigCredentials(ctx context.Context, cfg ports.PluginConfig, label string) (string, error) {
	cc, ok := cfg.(ports.CredentialedConfig)
	if !ok {
		return "", nil
	}
	uri := cc.CredentialsURI()
	if uri == "" {
		return "", nil
	}
	if b.credStore == nil {
		return "", fmt.Errorf("bridge: %s: credentials_uri specified but no credential store registered", label)
	}
	creds, err := b.credStore.Resolve(ctx, uri)
	if err != nil {
		return "", fmt.Errorf("bridge: %s: resolve credentials: %w", label, err)
	}
	if err := cc.ApplyCredentials(creds); err != nil {
		return "", fmt.Errorf("bridge: %s: apply credentials: %w", label, err)
	}
	return uri, nil
}
