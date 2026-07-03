package infra

import "testing"

func TestServiceProps_Normalized_AppliesDefaults(t *testing.T) {
	p := ServiceProps{
		ServiceName: "svc",
		Image:       "img:latest",
		Bootstrap: BootstrapConfig{
			BridgeID:         "b",
			ConfigFilePath:   "/f",
			AdminAPIKeyParam: "/a",
		},
	}.Normalized()

	assertEqual(t, 1, p.Replicas)
	assertEqual(t, 512, p.CPU)
	assertEqual(t, 1024, p.MemoryMiB)
	assertEqual(t, "/var/lib/gobridge", p.ConfigMountPath)
	assertEqual(t, "/gobridge", p.AccessPointPath)
}

func TestServiceProps_Normalized_PreservesExplicit(t *testing.T) {
	p := ServiceProps{
		ServiceName:     "svc",
		Image:           "img",
		Replicas:        3,
		CPU:             1024,
		MemoryMiB:       2048,
		ConfigMountPath: "/custom",
		AccessPointPath: "/ap",
		Bootstrap: BootstrapConfig{
			BridgeID:         "b",
			ConfigFilePath:   "/f",
			AdminAPIKeyParam: "/a",
			NodeRole:         NodeRoleWorker,
		},
	}.Normalized()

	assertEqual(t, 3, p.Replicas)
	assertEqual(t, 1024, p.CPU)
	assertEqual(t, 2048, p.MemoryMiB)
	assertEqual(t, "/custom", p.ConfigMountPath)
	assertEqual(t, NodeRoleWorker, p.Bootstrap.NodeRole)
}

func TestServiceProps_Validate_RequiresServiceName(t *testing.T) {
	p := ServiceProps{Image: "img", Bootstrap: BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a"}}.Normalized()
	p.ServiceName = ""
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	assertContains(t, err.Error(), "service_name")
}

func TestServiceProps_Validate_RequiresImage(t *testing.T) {
	p := ServiceProps{ServiceName: "svc", Bootstrap: BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a"}}.Normalized()
	p.Image = ""
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	assertContains(t, err.Error(), "image")
}

func TestServiceProps_Validate_RejectsUnNormalized(t *testing.T) {
	p := ServiceProps{
		ServiceName: "svc",
		Image:       "img",
		Bootstrap:   BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a"},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error for un-normalized props (replicas/cpu/memory = 0)")
	}
}

func TestServiceProps_Validate_OK(t *testing.T) {
	p := ServiceProps{
		ServiceName: "svc",
		Image:       "img",
		Bootstrap:   BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a"},
	}.Normalized()
	if err := p.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAppSpec_Normalized(t *testing.T) {
	s := AppSpec{
		StackName: "stack",
		Service: ServiceProps{
			ServiceName: "svc",
			Image:       "img",
			Bootstrap:   BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a"},
		},
	}.Normalized()
	assertEqual(t, 1, s.Service.Replicas)
}

func TestAppSpec_Validate_RequiresStackName(t *testing.T) {
	s := AppSpec{
		Service: ServiceProps{
			ServiceName: "svc",
			Image:       "img",
			Bootstrap:   BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a"},
		},
	}.Normalized()
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	assertContains(t, err.Error(), "stack_name")
}

func TestAppSpec_Validate_OK(t *testing.T) {
	s := AppSpec{
		StackName: "stack",
		Service: ServiceProps{
			ServiceName: "svc",
			Image:       "img",
			Bootstrap:   BootstrapConfig{BridgeID: "b", ConfigFilePath: "/f", AdminAPIKeyParam: "/a"},
		},
	}.Normalized()
	if err := s.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
