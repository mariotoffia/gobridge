//go:build integration_local

package integration

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/mariotoffia/gobridge/deployment/aws/infra"
)

func storageList(properties map[string]any, name string) ([]any, error) {
	value, present := properties[name]
	if !present {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("declares unreadable %s: %v", name, value)
	}
	return list, nil
}

// volumeFreeBootstrap admits no implicit file fallback. Before deployment CDK
// joins the bootstrap JSON around table-name Refs; preserve those as markers
// for shape validation only. The template itself is never changed here.
func volumeFreeBootstrap(value any) (infra.BootstrapConfig, error) {
	var boot infra.BootstrapConfig
	text, literal := value.(string)
	if !literal {
		intrinsic, _ := value.(map[string]any)
		join := asList(intrinsic["Fn::Join"])
		if len(join) != 2 || join[0] != "" {
			return boot, fmt.Errorf("volume-free task has no readable bootstrap document")
		}
		for _, part := range asList(join[1]) {
			if s, ok := part.(string); ok {
				text += s
				continue
			}
			ref, _ := part.(map[string]any)
			name, _ := ref["Ref"].(string)
			if len(ref) != 1 || name == "" {
				return boot, fmt.Errorf("unsupported bootstrap token: %v", part)
			}
			text += "Ref:" + name
		}
	}
	if err := json.Unmarshal([]byte(text), &boot); err != nil {
		return boot, fmt.Errorf("parse volume-free bootstrap: %w", err)
	}
	if boot.ConfigSource != infra.ConfigSourceDynamoDB || boot.ConfigFilePath != "" ||
		boot.ConfigDynamoDB == nil || strings.TrimSpace(boot.ConfigDynamoDB.TableName) == "" {
		return boot, fmt.Errorf("no shared storage: requires explicit DynamoDB config table and no config file path")
	}
	return boot, nil
}

// verifyVolumeFreeTask checks what ECS received before skipping restoration.
// Missing/unknown storage must not silently become a DynamoDB-only deployment.
func verifyVolumeFreeTask(definition *ecstypes.TaskDefinition) error {
	if definition == nil || len(definition.Volumes) != 0 || countMountPoints(definition) != 0 {
		return fmt.Errorf("expected a task definition without volumes or mounts")
	}
	found := 0
	for _, container := range definition.ContainerDefinitions {
		for _, pair := range container.Environment {
			if aws.ToString(pair.Name) != bootstrapDocumentVariable {
				continue
			}
			if _, err := volumeFreeBootstrap(aws.ToString(pair.Value)); err != nil {
				return err
			}
			found++
		}
	}
	if found != 1 {
		return fmt.Errorf("expected one DynamoDB bootstrap document, got %d", found)
	}
	return nil
}
