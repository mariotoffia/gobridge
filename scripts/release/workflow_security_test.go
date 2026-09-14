package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The release workflow publishes module tags only. It builds no container
// image, so the privileged boundary that remains is the one job that can write
// repository contents: the GitHub Release. It must stay free of checkout, Make
// and Docker, because a release body is written from validated job outputs
// rather than from anything the tagged source could influence.
func TestReleaseWorkflow_SeparatesPrivilegedJobs(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(repositoryRootForTest(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	workflow := string(data)
	release := workflowJobBlock(t, workflow, "github-release", "")

	requireWorkflowText(t, release, "contents: write", "actions/github-script@", "softprops/action-gh-release@")
	forbidWorkflowText(t, release, "actions/checkout", "make ", "docker", "trivy")

	for _, name := range []string{
		"tag-policy",
		"validate",
		"external-consumer-smoke",
		"github-release",
	} {
		block := workflowJobBlock(t, workflow, name, nextReleaseJob(name))
		if strings.Contains(block, "packages: write") {
			t.Errorf("job %s requests packages:write, but nothing publishes a container image", name)
		}
		if strings.Contains(block, "contents: write") && strings.Contains(block, "packages: write") {
			t.Errorf("job %s combines contents:write with packages:write", name)
		}
	}
}

// Nothing in the release path may reintroduce image publication: no registry
// login, no buildx, no scanner, no digest asset.
func TestReleaseWorkflow_PublishesNoContainerImage(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(repositoryRootForTest(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	forbidWorkflowText(
		t,
		string(data),
		"docker/login-action",
		"docker/setup-buildx-action",
		"docker/setup-qemu-action",
		"docker/build-push-action",
		"aquasecurity/trivy-action",
		"gobridge-image-digest.txt",
		"push-by-digest",
		"ghcr.io",
	)
}

func workflowJobBlock(t *testing.T, workflow, name, next string) string {
	t.Helper()

	startMarker := "\n  " + name + ":"
	start := strings.Index(workflow, startMarker)
	if start < 0 {
		t.Fatalf("workflow job %s is missing", name)
	}
	end := len(workflow)
	if next != "" {
		endMarker := "\n  " + next + ":"
		if found := strings.Index(workflow[start+len(startMarker):], endMarker); found >= 0 {
			end = start + len(startMarker) + found
		} else {
			t.Fatalf("workflow job %s does not precede %s", name, next)
		}
	}
	return workflow[start:end]
}

func requireWorkflowText(t *testing.T, block string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(block, value) {
			t.Errorf("workflow block missing %q", value)
		}
	}
}

func forbidWorkflowText(t *testing.T, block string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(block, value) {
			t.Errorf("workflow block contains forbidden %q", value)
		}
	}
}

func nextReleaseJob(name string) string {
	order := []string{
		"tag-policy",
		"validate",
		"external-consumer-smoke",
		"github-release",
	}
	for index, candidate := range order {
		if candidate == name && index+1 < len(order) {
			return order[index+1]
		}
	}
	return ""
}
