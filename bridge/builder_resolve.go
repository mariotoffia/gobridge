package bridge

import (
	"context"
	"fmt"
	"maps"

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

func (b *Builder) resolveCredentials(ctx context.Context, opts map[string]any, label string) (map[string]any, error) {
	uriVal, hasURI := opts["credentials_uri"]
	if !hasURI {
		return opts, nil
	}

	uri, ok := uriVal.(string)
	if !ok {
		return nil, fmt.Errorf("bridge: %s: credentials_uri must be a string", label)
	}

	if b.credStore == nil {
		return nil, fmt.Errorf("bridge: %s: credentials_uri specified but no credential store registered", label)
	}

	creds, err := b.credStore.Resolve(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("bridge: %s: resolve credentials: %w", label, err)
	}

	resolved := make(map[string]any, len(opts))
	maps.Copy(resolved, opts)
	delete(resolved, "credentials_uri")

	if creds.Password != nil {
		if _, exists := resolved["username"]; !exists {
			resolved["username"] = creds.Password.Username
		}
		if _, exists := resolved["password"]; !exists {
			resolved["password"] = creds.Password.Password
		}
	}
	if creds.TLS != nil {
		if _, exists := resolved["tls_cert"]; !exists {
			resolved["tls_cert"] = creds.TLS.CertPEM
		}
		if _, exists := resolved["tls_key"]; !exists {
			resolved["tls_key"] = creds.TLS.KeyPEM
		}
		if _, exists := resolved["tls_ca"]; !exists && len(creds.TLS.CAPEMs) > 0 {
			resolved["tls_ca"] = creds.TLS.CAPEMs
		}
		if _, exists := resolved["tls_insecure"]; !exists && creds.TLS.InsecureSkipVerify {
			resolved["tls_insecure"] = true
		}
	}

	return resolved, nil
}
