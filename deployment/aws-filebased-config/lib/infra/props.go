package infra

import (
	"fmt"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib/model"
)

type Exposure struct {
	Admin         bool `json:"admin,omitempty"`
	Monitor       bool `json:"monitor,omitempty"`
	TransportHTTP bool `json:"transport_http,omitempty"`
}

type ServiceProps struct {
	ServiceName string `json:"service_name"`
	Image       string `json:"image"`
	Replicas    int    `json:"replicas,omitempty"`

	CPU       int `json:"cpu,omitempty"`
	MemoryMiB int `json:"memory_mib,omitempty"`

	ConfigMountPath string `json:"config_mount_path,omitempty"`
	AccessPointPath string `json:"access_point_path,omitempty"`

	Bootstrap model.BootstrapConfig `json:"bootstrap"`
	Exposure  Exposure              `json:"exposure,omitempty"`
}

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

func (p ServiceProps) Validate() error {
	if p.ServiceName == "" {
		return fmt.Errorf("infra: service_name is required")
	}
	if p.Image == "" {
		return fmt.Errorf("infra: image is required")
	}
	return p.Bootstrap.Validate()
}

type AppSpec struct {
	StackName string       `json:"stack_name"`
	Service   ServiceProps `json:"service"`
}

func (s AppSpec) Normalized() AppSpec {
	out := s
	out.Service = out.Service.Normalized()
	return out
}

func (s AppSpec) Validate() error {
	if s.StackName == "" {
		return fmt.Errorf("infra: stack_name is required")
	}
	return s.Service.Validate()
}
