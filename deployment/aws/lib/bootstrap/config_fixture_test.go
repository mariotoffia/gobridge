package bootstrap

import (
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws/infra"
	"github.com/mariotoffia/gobridge/ports"
)

func defaultLogicalConfig(cfg deployinfra.BootstrapConfig) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:              cfg.BridgeID,
			DeploymentMode:  "standalone",
			ShutdownTimeout: "30s",
			DrainTimeout:    "30s",
		},
	}
}
