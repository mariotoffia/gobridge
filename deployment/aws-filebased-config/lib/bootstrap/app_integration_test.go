package bootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/testutil/localstack"
	"github.com/stretchr/testify/require"
)

func TestIntegration_AppStartsWithLocalstackSSMSecrets(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	localstack.Configure(
		localstack.WithServices("ssm"),
		localstack.WithCleanOrphans(true),
	)
	t.Cleanup(localstack.Shutdown)

	ssmClient := localstack.SSMClient(t)
	putSecureString(t, ssmClient, "/gobridge/admin", "admin-secret-key-123456")
	putSecureString(t, ssmClient, "/gobridge/monitor", "monitor-secret-key-123")

	cfgPath := t.TempDir() + "/bridge.yaml"
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:           "bridge-integration",
		ConfigFilePath:     cfgPath,
		PollInterval:       "100ms",
		AdminAddr:          ":0",
		MonitorAddr:        ":0",
		TransportHTTPAddr:  ":0",
		AdminAPIKeyParam:   "/gobridge/admin",
		MonitorAPIKeyParam: "/gobridge/monitor",
		AWSRegion:          "us-east-1",
		SSMEndpoint:        localstack.Endpoint(t),
		DevMode:            true,
	})

	require.NoError(t, app.Start(t.Context()))
	t.Cleanup(func() {
		_ = app.Stop(context.Background())
	})

	req, err := http.NewRequest(http.MethodGet, app.AdminURL()+"/api/v1/admin/config", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", "admin-secret-key-123456")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.NotNil(t, body["config"])
}

func putSecureString(t *testing.T, client *awsssm.Client, name, value string) {
	t.Helper()
	_, err := client.PutParameter(t.Context(), &awsssm.PutParameterInput{
		Name:      aws.String(name),
		Value:     aws.String(value),
		Type:      ssmtypes.ParameterTypeSecureString,
		Overwrite: aws.Bool(true),
	})
	require.NoError(t, err)
}
