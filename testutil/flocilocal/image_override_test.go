package flocilocal

import (
	"os"
	"testing"
)

// FLOCI_IMAGE picks the emulator release a run is on, which is how a break is
// traced to the emulator: re-run on an earlier release and see whether it
// holds. Unset or empty, the helper stays on the moving default; set, the
// helper runs the named image instead. Pinned without Docker.
//
// Category: unit (TESTS.md §1).

const imageEnv = "FLOCI_IMAGE"

func TestImageName_UnsetOrEmptyRunsLatest(t *testing.T) {
	t.Setenv(imageEnv, "")
	if got := imageName(); got != "floci/floci:latest" {
		t.Fatalf("%s empty: image = %q, want floci/floci:latest", imageEnv, got)
	}
	// t.Setenv restores the caller's value when the test ends, so unsetting
	// the variable here leaks nothing into the next test.
	if err := os.Unsetenv(imageEnv); err != nil {
		t.Fatalf("unset %s: %v", imageEnv, err)
	}
	if got := imageName(); got != "floci/floci:latest" {
		t.Fatalf("%s unset: image = %q, want floci/floci:latest", imageEnv, got)
	}
}

func TestImageName_SetRunsTheOverride(t *testing.T) {
	const earlier = "floci/floci:2.0.1"
	t.Setenv(imageEnv, earlier)
	if got := imageName(); got != earlier {
		t.Fatalf("%s=%s: image = %q, want the override", imageEnv, earlier, got)
	}
	args, err := render()
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	if got := args[len(args)-1]; got != earlier {
		t.Fatalf("%s=%s, yet docker run starts %q: %q", imageEnv, earlier, got, args)
	}
}
