package infra

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib/model"
)

func TestServiceProps_Normalized_AppliesDefaults(t *testing.T) {
	p := ServiceProps{
		ServiceName: "my-svc",
		Image:       "myimage:latest",
		Bootstrap: model.BootstrapConfig{
			BridgeID:         "bridge-1",
			ConfigFilePath:   "/etc/gobridge/bridge.yaml",
			AdminAPIKeyParam: "/admin",
		},
	}.Normalized()

	assert.Equal(t, 1, p.Replicas)
	assert.Equal(t, 512, p.CPU)
	assert.Equal(t, 1024, p.MemoryMiB)
	assert.Equal(t, "/var/lib/gobridge", p.ConfigMountPath)
	assert.Equal(t, "/gobridge", p.AccessPointPath)
	assert.Equal(t, model.NodeRoleControl, p.Bootstrap.NodeRole)
	assert.Equal(t, model.TopologySingle, p.Bootstrap.Topology)
}

func TestServiceProps_Normalized_PreservesExplicitValues(t *testing.T) {
	p := ServiceProps{
		ServiceName:     "my-svc",
		Image:           "myimage:latest",
		Replicas:        3,
		CPU:             1024,
		MemoryMiB:       2048,
		ConfigMountPath: "/custom",
		AccessPointPath: "/custom-ap",
		Bootstrap: model.BootstrapConfig{
			BridgeID:         "bridge-1",
			ConfigFilePath:   "/etc/bridge.yaml",
			AdminAPIKeyParam: "/admin",
			NodeRole:         model.NodeRoleWorker,
			Topology:         model.TopologyFilesystemReplicated,
		},
	}.Normalized()

	assert.Equal(t, 3, p.Replicas)
	assert.Equal(t, 1024, p.CPU)
	assert.Equal(t, 2048, p.MemoryMiB)
	assert.Equal(t, "/custom", p.ConfigMountPath)
	assert.Equal(t, "/custom-ap", p.AccessPointPath)
	assert.Equal(t, model.NodeRoleWorker, p.Bootstrap.NodeRole)
}

func TestServiceProps_Validate_RequiresServiceName(t *testing.T) {
	p := ServiceProps{
		Image: "myimage:latest",
	}.Normalized()
	err := p.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service_name")
}

func TestServiceProps_Validate_RequiresImage(t *testing.T) {
	p := ServiceProps{
		ServiceName: "my-svc",
	}.Normalized()
	err := p.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image")
}

func TestServiceProps_Validate_RequiresBootstrapFields(t *testing.T) {
	p := ServiceProps{
		ServiceName: "my-svc",
		Image:       "myimage:latest",
	}.Normalized()
	err := p.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge_id")
}

func TestServiceProps_Validate_RejectsUnNormalized(t *testing.T) {
	p := ServiceProps{
		ServiceName: "my-svc",
		Image:       "myimage:latest",
		Bootstrap: model.BootstrapConfig{
			BridgeID:         "bridge-1",
			ConfigFilePath:   "/etc/bridge.yaml",
			AdminAPIKeyParam: "/admin",
		},
	}
	err := p.Validate()
	require.Error(t, err, "Validate without Normalized should fail on replicas/cpu/memory")
}

func TestAppSpec_Normalized_NormalizesService(t *testing.T) {
	s := AppSpec{
		StackName: "my-stack",
		Service: ServiceProps{
			ServiceName: "my-svc",
			Image:       "myimage:latest",
			Bootstrap: model.BootstrapConfig{
				BridgeID:         "bridge-1",
				ConfigFilePath:   "/etc/bridge.yaml",
				AdminAPIKeyParam: "/admin",
			},
		},
	}.Normalized()

	assert.Equal(t, 1, s.Service.Replicas)
	assert.Equal(t, 512, s.Service.CPU)
}

func TestAppSpec_Validate_RequiresStackName(t *testing.T) {
	s := AppSpec{
		Service: ServiceProps{
			ServiceName: "my-svc",
			Image:       "myimage:latest",
			Bootstrap: model.BootstrapConfig{
				BridgeID:         "bridge-1",
				ConfigFilePath:   "/etc/bridge.yaml",
				AdminAPIKeyParam: "/admin",
			},
		},
	}.Normalized()
	err := s.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stack_name")
}

func TestAppSpec_Validate_OK(t *testing.T) {
	s := AppSpec{
		StackName: "my-stack",
		Service: ServiceProps{
			ServiceName: "my-svc",
			Image:       "myimage:latest",
			Bootstrap: model.BootstrapConfig{
				BridgeID:         "bridge-1",
				ConfigFilePath:   "/etc/bridge.yaml",
				AdminAPIKeyParam: "/admin",
			},
		},
	}.Normalized()
	require.NoError(t, s.Validate())
}
