//go:build integration_aws || integration_local

package integration

import (
	"os"
	"strings"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
)

// Each credentialed fixture needs its own embedded document. A generic registry
// image cannot receive that document after it has been built.
//
//nolint:ireturn // BridgeImageSource is the sealed facade input.
func credentialedRuntimeImageSource() gobridgecdk.BridgeImageSource {
	if strings.TrimSpace(os.Getenv("GOBRIDGE_INT_IMAGE")) != "" {
		panic("integration: GOBRIDGE_INT_IMAGE is no longer supported; set GOBRIDGE_INT_VERSION to build each fixture with its embedded initial config")
	}
	version := strings.TrimSpace(os.Getenv("GOBRIDGE_INT_VERSION"))
	if version == "" {
		panic("integration: GOBRIDGE_INT_VERSION must name a published module version supporting embedded initial config")
	}
	return gobridgecdk.ImageFromGoBuild(gobridgecdk.ImageGoBuildProps{Version: version})
}
