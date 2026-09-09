package main

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

func admitObservedConfig(ctx context.Context, cfg *ports.BridgeConfig, control *ports.HTTPConfig, validator *bridge.Supervisor) error {
	if err := config.Validate(cfg); err != nil {
		return err
	}
	if cfg.HTTP != nil && (cfg.HTTP.TLSCertFile != "" || cfg.HTTP.TLSKeyFile != "") &&
		(cfg.HTTP.TLSCertFile != control.TLSCertFile || cfg.HTTP.TLSKeyFile != control.TLSKeyFile) {
		return fmt.Errorf("http TLS settings require matching process-owned listener settings")
	}
	if validator == nil {
		return fmt.Errorf("configuration admission is starting")
	}
	return validator.Preflight(ctx, cfg)
}
