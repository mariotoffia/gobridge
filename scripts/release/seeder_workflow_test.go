package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeederRelease_UsesValidatedTagAndDockerHubDigest(t *testing.T) {
	t.Parallel()

	repo := repositoryRootForTest(t)
	data, err := os.ReadFile(filepath.Join(repo, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	job := workflowJobBlock(t, string(data), "seeder-image", "github-release")
	requireWorkflowText(t, job,
		"- validate", "- external-consumer-smoke",
		"needs.validate.outputs.path == 'cmd/gobridge'",
		"persist-credentials: false",
		"vars.DOCKERHUB_USERNAME", "secrets.DOCKERHUB_TOKEN",
		"make verify-remote-release-tag",
		"context: deployment/aws-filebased-config/cdk/constructs/internal/seeder",
		"platforms: linux/amd64,linux/arm64",
		"push-by-digest=true", "gobridge-seeder",
		"actions/upload-artifact@",
	)
	forbidWorkflowText(t, job, "contents: write", "packages: write", "tags:", ":latest")

	data, err = os.ReadFile(filepath.Join(repo,
		"deployment/aws-filebased-config/cdk/constructs/internal/seeder/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	requireWorkflowText(t, string(data),
		"python3-pyyaml", "COPY seeder.sh seeder-ddb.sh",
		"bash /seeder/tests/run.sh",
	)
}
