package config

import (
	"github.com/mariotoffia/gobridge/ports"
)

// ─── Phase 1 test fakes ────────────────────────────────────────────
//
// These helpers exist only for the config package's own tests. They
// stand in for the real adapter-side decoders that PHASE 2 will
// migrate. Each registered kind decodes to a passthrough config that
// preserves the kind value and never errors on Validate.

// passthroughConfig is a minimal ports.PluginConfig used by tests
// that only care about kind plumbing, not adapter-specific fields.
type passthroughConfig struct {
	kind string
}

func (p passthroughConfig) Kind() string    { return p.kind }
func (p passthroughConfig) Validate() error { return nil }

// passthroughRegistry returns a fresh ports.Registry pre-populated
// with passthrough decoders for the requested kinds. The decoder
// ignores the raw payload's contents — its only job is to satisfy
// the registry lookup so existing high-level parser tests can use
// real-looking kind names like "mqtt", "sqs", "dynamodb" without
// pulling in any adapter packages.
func passthroughRegistry(kinds ...string) *ports.Registry {
	reg := ports.NewRegistry()
	for _, k := range kinds {
		k := k
		if err := reg.Register(k, func(_ ports.RawConfig) (ports.PluginConfig, error) {
			return passthroughConfig{kind: k}, nil
		}); err != nil {
			panic(err)
		}
	}
	return reg
}
