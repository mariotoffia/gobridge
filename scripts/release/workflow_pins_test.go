package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestWorkflows_BuildKitAndBinfmtPinsCannotSwap(t *testing.T) {
	t.Parallel()

	const (
		buildkit = "moby/buildkit:v0.31.1@sha256:6b59b7df63a8cb9902736f9ddf7fcff8261613d3e7449b8ea8b7537fc399c03a"
		binfmt   = "tonistiigi/binfmt:qemu-v10.2.3@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0"
	)
	buildkitPattern := regexp.MustCompile(`moby/buildkit:v0\.31\.1@sha256:[0-9a-f]{64}`)
	binfmtPattern := regexp.MustCompile(`tonistiigi/binfmt:qemu-v10\.2\.3@sha256:[0-9a-f]{64}`)

	buildkitPins := 0
	binfmtPins := 0
	for _, workflowName := range []string{"ci.yml", "release.yml"} {
		data, err := os.ReadFile(filepath.Join(
			repositoryRootForTest(t),
			".github",
			"workflows",
			workflowName,
		))
		if err != nil {
			t.Fatalf("read %s: %v", workflowName, err)
		}
		for _, pin := range buildkitPattern.FindAllString(string(data), -1) {
			buildkitPins++
			if pin != buildkit {
				t.Errorf("%s BuildKit pin = %q, want %q", workflowName, pin, buildkit)
			}
		}
		for _, pin := range binfmtPattern.FindAllString(string(data), -1) {
			binfmtPins++
			if pin != binfmt {
				t.Errorf("%s binfmt pin = %q, want %q", workflowName, pin, binfmt)
			}
		}
	}
	if buildkitPins != 2 {
		t.Errorf("BuildKit pin count = %d, want one in CI and one in release", buildkitPins)
	}
	if binfmtPins != 1 {
		t.Errorf("binfmt pin count = %d, want one in release", binfmtPins)
	}
}
