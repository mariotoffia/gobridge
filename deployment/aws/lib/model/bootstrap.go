// Package model preserves the original bootstrap-model import path as aliases
// to the canonical zero-dependency deployment infra module. Keeping one type
// and one default set prevents the runtime and CDK bootstrap contracts from
// drifting.
package model

import deployinfra "github.com/mariotoffia/gobridge/deployment/aws/infra"

type NodeRole = deployinfra.NodeRole

const (
	NodeRoleControl = deployinfra.NodeRoleControl
	NodeRoleWorker  = deployinfra.NodeRoleWorker
)

type Topology = deployinfra.Topology

const (
	TopologySingle                = deployinfra.TopologySingle
	TopologyFilesystemReplicated  = deployinfra.TopologyFilesystemReplicated
	TopologyDynamoDBCoordinatedHA = deployinfra.TopologyDynamoDBCoordinatedHA
)

const (
	ConfigSourceFile     = deployinfra.ConfigSourceFile
	ConfigSourceDynamoDB = deployinfra.ConfigSourceDynamoDB
)

type ConfigDynamoDBSettings = deployinfra.ConfigDynamoDBSettings

const (
	DefaultAdminAddr            = deployinfra.DefaultAdminAddr
	DefaultMonitorAddr          = deployinfra.DefaultMonitorAddr
	DefaultTransportHTTPAddr    = deployinfra.DefaultTransportHTTPAddr
	DefaultPollInterval         = deployinfra.DefaultPollInterval
	DefaultDynamoDBPollInterval = deployinfra.DefaultDynamoDBPollInterval
	DefaultMountPath            = deployinfra.DefaultMountPath
	DefaultContainerMemoryBytes = deployinfra.DefaultContainerMemoryBytes
)

type BootstrapConfig = deployinfra.BootstrapConfig
