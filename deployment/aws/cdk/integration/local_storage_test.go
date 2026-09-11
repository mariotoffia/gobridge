//go:build integration_local

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/stretchr/testify/require"
)

// Missing storage is valid only when the task explicitly reads its config from
// DynamoDB. Treating an unreadable declaration as an empty list would hide a
// broken file deployment behind the DynamoDB exception.
func TestDeclaredTaskSpec_ConfigStorage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bootstrap any
		volumes   any
		mounts    any
		wantError bool
	}{
		{name: "dynamodb_without_filesystem", bootstrap: `{"config_source":"dynamodb","config_dynamodb":{"table_name":"config-table"}}`},
		{name: "dynamodb_table_token", bootstrap: map[string]any{"Fn::Join": []any{"", []any{
			`{"config_source":"dynamodb","config_dynamodb":{"table_name":"`, map[string]any{"Ref": "ConfigTable"}, `"}}`,
		}}}},
		{name: "file_requires_storage", bootstrap: `{"config_source":"file","config_file_path":"/config/bridge.yaml"}`, wantError: true},
		{name: "default_requires_storage", bootstrap: `{}`, wantError: true},
		{name: "unknown_source", bootstrap: `{"config_source":"other"}`, wantError: true},
		{name: "missing_bootstrap", wantError: true},
		{name: "invalid_bootstrap", bootstrap: `not json`, wantError: true},
		{name: "missing_table", bootstrap: `{"config_source":"dynamodb"}`, wantError: true},
		{name: "file_path_on_volume_free_task", bootstrap: `{"config_source":"dynamodb","config_file_path":"/config/bridge.yaml","config_dynamodb":{"table_name":"config-table"}}`, wantError: true},
		{name: "malformed_volumes", bootstrap: `{"config_source":"dynamodb","config_dynamodb":{"table_name":"config-table"}}`, volumes: "broken", wantError: true},
		{name: "malformed_mounts", bootstrap: `{"config_source":"dynamodb","config_dynamodb":{"table_name":"config-table"}}`, mounts: "broken", wantError: true},
		{name: "unknown_volume", volumes: []any{map[string]any{"Name": "config", "DockerVolumeConfiguration": map[string]any{}}}, wantError: true},
		{name: "file_bind_mount", bootstrap: `{"config_source":"file"}`,
			volumes: []any{map[string]any{"Name": "config", "Host": map[string]any{"SourcePath": "/local/config"}}},
			mounts:  []any{map[string]any{"SourceVolume": "config", "ContainerPath": "/config", "ReadOnly": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			container := map[string]any{"Name": "bridge"}
			if tc.bootstrap != nil {
				container["Environment"] = []any{map[string]any{"Name": bootstrapDocumentVariable, "Value": tc.bootstrap}}
			}
			if tc.mounts != nil {
				container["MountPoints"] = tc.mounts
			}
			props := map[string]any{"Family": "bridge-family", "ContainerDefinitions": []any{container}}
			if tc.volumes != nil {
				props["Volumes"] = tc.volumes
			}
			family, spec, err := declaredTaskSpec(props)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "bridge-family", family)
			if tc.volumes != nil {
				require.Equal(t, []string{"config"}, spec.Volumes)
				require.Equal(t, []localMountSpec{{Container: "bridge", SourceVolume: "config", ContainerPath: "/config", ReadOnly: true}}, spec.Mounts)
			} else {
				require.Empty(t, spec.Volumes)
				require.Empty(t, spec.Mounts)
			}
		})
	}
}

// The restore must verify ECS's definition before leaving it untouched. Missing
// bootstrap, a file fallback, or any deployed mount must not take that shortcut.
func TestVerifyVolumeFreeTask(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bootstrap string
		volume    bool
		mount     bool
		wantError bool
	}{
		{name: "dynamodb", bootstrap: `{"config_source":"dynamodb","config_dynamodb":{"table_name":"config-table"}}`},
		{name: "missing_bootstrap", wantError: true},
		{name: "file_fallback", bootstrap: `{}`, wantError: true},
		{name: "unexpected_volume", volume: true, wantError: true},
		{name: "unexpected_mount", mount: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			container := ecstypes.ContainerDefinition{Name: aws.String("bridge")}
			if tc.bootstrap != "" {
				container.Environment = []ecstypes.KeyValuePair{{Name: aws.String(bootstrapDocumentVariable), Value: aws.String(tc.bootstrap)}}
			}
			if tc.mount {
				container.MountPoints = []ecstypes.MountPoint{{SourceVolume: aws.String("config")}}
			}
			definition := &ecstypes.TaskDefinition{ContainerDefinitions: []ecstypes.ContainerDefinition{container}}
			if tc.volume {
				definition.Volumes = []ecstypes.Volume{{Name: aws.String("config")}}
			}
			err := verifyVolumeFreeTask(definition)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
	require.Error(t, verifyVolumeFreeTask(nil))
}
