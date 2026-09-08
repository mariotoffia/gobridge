package validation

import (
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/stretchr/testify/require"
)

// TestPhase1_DynamoDBConfig_FilesystemProfile verifies replicated filesystems require a file source.
func TestPhase1_DynamoDBConfig_FilesystemProfile(t *testing.T) {
	in := baseInput(baseConfig())
	in.Bootstrap.Topology = infra.TopologyFilesystemReplicated
	in.Bootstrap.ConfigSource = infra.ConfigSourceDynamoDB
	var profileErr *ErrFilesystemProfile
	require.ErrorAs(t, Phase1(in), &profileErr)
}
