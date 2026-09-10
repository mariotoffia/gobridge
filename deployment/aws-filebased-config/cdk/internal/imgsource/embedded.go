package imgsource

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/ports"
)

type initialConfigAsset struct {
	name   string
	data   []byte
	digest string
}

func prepareInitialConfig(cfg *ports.BridgeConfig) *initialConfigAsset {
	if cfg == nil {
		return nil
	}
	if err := bridgecfg.ValidateEmbeddedSQSConfig(cfg); err != nil {
		panic(fmt.Sprintf("gobridgecdk: invalid embedded initial config: %v", err))
	}
	data, err := parser.MarshalYAML(cfg)
	if err != nil {
		panic(fmt.Sprintf("gobridgecdk: serialize embedded initial config: %v", err))
	}
	if unresolved := awscdk.Token_IsUnresolved(string(data)); unresolved != nil && *unresolved {
		panic("gobridgecdk: embedded initial config contains unresolved CDK tokens; use stable names or selectors")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	return &initialConfigAsset{
		name:   "initial-config-" + digest + ".base64",
		data:   []byte(base64.StdEncoding.EncodeToString(data)),
		digest: digest,
	}
}
