package main

import (
	"context"
	"errors"
	"log/slog"

	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// configLoader returns the process config loader. With startEmpty enabled a
// MISSING file falls back to an empty configuration; with it disabled the
// unwrapped file source is returned, so a missing file is the fatal load error
// it would be for any other unreadable config. Refusing the fallback is the
// posture a deployment wants when carrying zero routes is never correct — a
// mistyped path or an unmounted volume should fail the process, not boot a
// bridge that silently transports nothing.
func configLoader(src *fileconfig.Source, path string, startEmpty bool, logger *slog.Logger) ports.Loader { //nolint:ireturn // intentional: the caller depends on the ports.Loader driven-port interface, not on which of the two loaders it got
	if !startEmpty {
		return src
	}
	return &startEmptySource{src: src, path: path, logger: logger}
}

// startEmptySource wraps the file config source so a MISSING config file
// starts an empty, healthy bridge instead of failing the process (start-empty,
// mirroring the deployment profile's optionalFileSource). The fallback is
// loud: a missing file is warned on every load so an operator who mistyped
// -config sees why nothing is bridged. Only shared.ErrNotFound falls back —
// an unreadable or unparseable file stays a fatal load error, because
// silently replacing a config that EXISTS but cannot be read would swap real
// routes for none.
type startEmptySource struct {
	src    *fileconfig.Source
	path   string
	logger *slog.Logger
}

var _ ports.Loader = (*startEmptySource)(nil)

func (s *startEmptySource) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	cfg, err := s.src.Load(ctx)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, shared.ErrNotFound) {
		// The recovery path named here must be one that exists. The start-empty
		// config defines no HTTP block and this root binds its listeners once
		// from the boot config, so there is no admin API and no probe port to
		// push a config through: creating the file is what converges a running
		// process, and anything that needs listeners needs a restart.
		s.logger.Warn(
			"config file not found; starting empty — no routes are bridged and, "+
				"because the empty configuration defines no http block, this process "+
				"serves no admin API and no health probes. Create the file to converge "+
				"the routes, and restart the process to bring up the HTTP listeners",
			"path", s.path,
		)
		return defaultEmptyConfig(), nil
	}
	return nil, err
}

// defaultEmptyConfig is the start-empty logical config: the one field
// validation requires (bridge.id) plus the same explicit shutdown budgets the
// deployment profile's defaultLogicalConfig sets. Zero routes — the bridge is
// a no-op until configured.
func defaultEmptyConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:              "gobridge",
			DeploymentMode:  "standalone",
			ShutdownTimeout: "30s",
			DrainTimeout:    "30s",
		},
	}
}
