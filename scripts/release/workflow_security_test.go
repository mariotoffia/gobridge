package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseWorkflow_SeparatesPrivilegedJobs(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(repositoryRootForTest(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	workflow := string(data)
	release := workflowJobBlock(t, workflow, "github-release", "image")
	image := workflowJobBlock(t, workflow, "image", "image-association")
	association := workflowJobBlock(t, workflow, "image-association", "latest-promotion")
	latest := workflowJobBlock(t, workflow, "latest-promotion", "")

	requireWorkflowText(t, release, "contents: write", "actions/github-script@", "softprops/action-gh-release@")
	forbidWorkflowText(t, release, "actions/checkout", "make ", "docker", "trivy")

	requireWorkflowText(t, image, "contents: read", "packages: write", "persist-credentials: false")
	forbidWorkflowText(t, image, "contents: write")

	requireWorkflowText(t, association, "contents: write", "needs:", "- image", "actions/github-script@")
	forbidWorkflowText(
		t,
		association,
		"packages: write",
		"docker",
		"trivy",
		"setup-qemu",
		"setup-buildx",
		"build-push",
		"actions/checkout",
		"go run",
		"make ",
	)

	requireWorkflowText(
		t,
		latest,
		"contents: read",
		"packages: write",
		"- image-association",
		"persist-credentials: false",
	)
	forbidWorkflowText(t, latest, "contents: write", "build-push", "trivy", "setup-qemu")

	for _, name := range []string{
		"tag-policy",
		"validate",
		"final-release-tests",
		"external-consumer-smoke",
		"github-release",
		"image",
		"image-association",
		"latest-promotion",
	} {
		block := workflowJobBlock(t, workflow, name, nextReleaseJob(name))
		if strings.Contains(block, "contents: write") && strings.Contains(block, "packages: write") {
			t.Errorf("job %s combines contents:write with packages:write", name)
		}
	}
}

func TestReleaseWorkflow_FinalCommandTestsGatePublication(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(repositoryRootForTest(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	workflow := string(data)
	gate := workflowJobBlock(t, workflow, "final-release-tests", "external-consumer-smoke")
	requireWorkflowText(
		t,
		gate,
		"needs: validate",
		"if: needs.validate.outputs.path != 'cmd/gobridge'",
		"if: needs.validate.outputs.path == 'cmd/gobridge'",
		"- name: Run uncached integration release gate\n"+
			"        if: needs.validate.outputs.path == 'cmd/gobridge'\n"+
			"        run: make test-integration",
		"- name: Run uncached long-running release gate\n"+
			"        if: needs.validate.outputs.path == 'cmd/gobridge'\n"+
			"        run: make test-long-running",
		"- name: Run bounded MQTT ingress memory release gate\n"+
			"        if: needs.validate.outputs.path == 'cmd/gobridge'\n"+
			"        run: make test-mqtt-ingress-memory",
	)
	forbidWorkflowText(t, gate, "\n    if:",
		"make test-integration ||", "make test-long-running ||")

	for _, downstream := range []struct {
		name string
		next string
	}{
		{name: "external-consumer-smoke", next: "github-release"},
		{name: "github-release", next: "image"},
		{name: "image", next: "image-association"},
	} {
		block := workflowJobBlock(t, workflow, downstream.name, downstream.next)
		requireWorkflowText(t, block, "needs:", "- final-release-tests")
	}
}

func TestReleaseWorkflow_ResumesAndRescansRecordedDigest(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(repositoryRootForTest(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	image := workflowJobBlock(t, string(data), "image", "image-association")
	requireWorkflowText(
		t,
		image,
		"- name: Build and push image content by digest\n"+
			"        if: steps.association.outputs.exists != 'true'\n"+
			"        id: build",
		`digest="$RECORDED_DIGEST"`,
		`echo "mode=resumed" >> "$GITHUB_OUTPUT"`,
		`docker buildx imagetools inspect "$IMAGE@$DIGEST" --raw`,
	)
	forbidWorkflowText(
		t,
		image,
		`[ "$RECORDED_DIGEST" != "$BUILT_DIGEST" ]`,
		"recorded release digest differs from rebuilt tagged source",
		"mode=rebuilt-verified",
	)
	login := strings.Index(image, "Log in to GHCR")
	association := strings.Index(image, "Fetch existing command release image association")
	build := strings.Index(image, "Build and push image content by digest")
	selectDigest := strings.Index(image, "Select immutable image digest")
	inspect := strings.Index(image, "Inspect image platform children")
	if login < 0 || association < login || build < association ||
		selectDigest < build || inspect < selectDigest {
		t.Error("image association must control the optional build before digest selection and inspection")
	}
}

func TestReleaseWorkflow_RejectsUnscannedRunnableChildren(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(repositoryRootForTest(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	image := workflowJobBlock(t, string(data), "image", "image-association")
	requireWorkflowText(
		t,
		image,
		`runnable_count="$(jq '[.manifests[] |`,
		`select(.platform.os != "unknown" or .platform.architecture != "unknown")] |`,
		`if [ "$runnable_count" -ne 2 ]`,
	)
	if strings.Contains(image, `select(.annotations["vnd.docker.reference.type"] != "attestation-manifest")`) {
		t.Fatal("release workflow trusts mutable annotations to classify non-runnable image children")
	}
}

func TestWorkflows_BoundedMQTTIngressMemoryUsesSharedTarget(t *testing.T) {
	t.Parallel()

	root := repositoryRootForTest(t)
	ciData, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	releaseData, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	ciIntegration := workflowJobBlock(t, string(ciData), "integration", "long-running")
	releaseGate := workflowJobBlock(t, string(releaseData), "final-release-tests", "external-consumer-smoke")
	requireWorkflowText(t, ciIntegration,
		"- name: Run bounded MQTT ingress memory proof\n"+
			"        run: make test-mqtt-ingress-memory")
	requireWorkflowText(t, releaseGate, "run: make test-mqtt-ingress-memory")
	forbidWorkflowText(t, ciIntegration,
		"GOBRIDGE_REQUIRE_MEMORY_LIMIT",
		"memory.max",
		"mqtt-memory.test",
		"docker network create",
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
		"final-release-tests",
		"external-consumer-smoke",
		"github-release",
		"image",
		"image-association",
		"latest-promotion",
	}
	for index, candidate := range order {
		if candidate == name && index+1 < len(order) {
			return order[index+1]
		}
	}
	return ""
}
