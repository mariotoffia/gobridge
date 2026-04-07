package infra

import "fmt"

// Exposure controls which HTTP ports the service exposes externally.
type Exposure struct {
	Admin         bool `json:"admin,omitempty"`
	Monitor       bool `json:"monitor,omitempty"`
	TransportHTTP bool `json:"transport_http,omitempty"`
}

// ServiceProps defines the container-level and bootstrap properties
// for a gobridge file-based deployment. Used by CDK constructs to
// create ECS task definitions and by the spec validator to normalize
// deployment specifications.
type ServiceProps struct {
	ServiceName string `json:"service_name"`
	Image       string `json:"image"`
	Replicas    int    `json:"replicas,omitempty"`

	CPU       int `json:"cpu,omitempty"`
	MemoryMiB int `json:"memory_mib,omitempty"`

	ConfigMountPath string `json:"config_mount_path,omitempty"`
	AccessPointPath string `json:"access_point_path,omitempty"`

	Bootstrap BootstrapConfig `json:"bootstrap"`
	Exposure  Exposure        `json:"exposure,omitempty"`
}

// Normalized returns a copy with defaults applied for any unset fields.
func (p ServiceProps) Normalized() ServiceProps {
	out := p
	if out.Replicas <= 0 {
		out.Replicas = 1
	}
	if out.CPU <= 0 {
		out.CPU = 512
	}
	if out.MemoryMiB <= 0 {
		out.MemoryMiB = 1024
	}
	if out.ConfigMountPath == "" {
		out.ConfigMountPath = "/mnt/gobridge"
	}
	if out.AccessPointPath == "" {
		out.AccessPointPath = "/gobridge"
	}
	out.Bootstrap = out.Bootstrap.Normalized()
	return out
}

// Validate checks that all required fields are set and values are in range.
// Call Normalized() before Validate() to fill in defaults.
func (p ServiceProps) Validate() error {
	if p.ServiceName == "" {
		return fmt.Errorf("infra: service_name is required")
	}
	if p.Image == "" {
		return fmt.Errorf("infra: image is required")
	}
	if p.Replicas <= 0 {
		return fmt.Errorf("infra: replicas must be > 0 (got %d; call Normalized() first)", p.Replicas)
	}
	if p.CPU <= 0 {
		return fmt.Errorf("infra: cpu must be > 0 (got %d; call Normalized() first)", p.CPU)
	}
	if p.MemoryMiB <= 0 {
		return fmt.Errorf("infra: memory_mib must be > 0 (got %d; call Normalized() first)", p.MemoryMiB)
	}
	return p.Bootstrap.Validate()
}

// AppSpec is the top-level deployment specification for the CDK spec
// validator. It combines a CloudFormation stack name with service properties.
type AppSpec struct {
	StackName string       `json:"stack_name"`
	Service   ServiceProps `json:"service"`
}

// Normalized returns a copy with defaults applied.
func (s AppSpec) Normalized() AppSpec {
	out := s
	out.Service = out.Service.Normalized()
	return out
}

// Validate checks that all required fields are set.
func (s AppSpec) Validate() error {
	if s.StackName == "" {
		return fmt.Errorf("infra: stack_name is required")
	}
	return s.Service.Validate()
}
