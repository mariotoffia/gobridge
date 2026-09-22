package flocilocal

import (
	"slices"
	"strings"
	"testing"
)

// Floci refuses an ECS task definition whose host volume lies outside an
// approved root, so a deployment test must be able to approve the directories
// it mounts from, and a test that asked for none must approve none. Floci reads
// the roots comma-separated, so a root containing a comma must fail the fixture
// rather than approve its pieces. All three are pinned at render time, without
// Docker.
//
// Category: unit (TESTS.md §1).

const hostVolumeRootsSetting = "FLOCI_SERVICES_ECS_HOST_VOLUME_ROOTS="

// render builds the docker run arguments with fns applied, as Configure and
// startContainer do.
func render(fns ...Option) ([]string, error) {
	var o options
	for _, fn := range fns {
		fn(&o)
	}
	return runArgs(containerPrefix+"unit", gatewayPort, o)
}

func TestRunArgs_NoHostVolumeRootsApprovesNone(t *testing.T) {
	args, err := render()
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, hostVolumeRootsSetting) {
			t.Fatalf("no root was asked for, yet the emulator is started with %q: %q", arg, args)
		}
	}
}

func TestRunArgs_HostVolumeRootsApproveExactlyTheNamedDirectories(t *testing.T) {
	args, err := render(WithHostVolumeRoots("/tmp/run-a", "/tmp/run-b"))
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	at := slices.Index(args, hostVolumeRootsSetting+"/tmp/run-a,/tmp/run-b")
	if at < 1 || args[at-1] != "-e" {
		t.Fatalf("the roots are not passed as floci's comma-separated setting: %q", args)
	}
	if args[len(args)-1] != imageName() {
		t.Fatalf("the image is not the last argument, so docker would hand the setting to the container as its command: %q", args)
	}
}

func TestRunArgs_HostVolumeRootWithACommaIsRejected(t *testing.T) {
	if args, err := render(WithHostVolumeRoots("/tmp/a,b")); err == nil {
		t.Fatalf("a root floci would split into /tmp/a and b was accepted: %q", args)
	}
}

// TestRunArgs_HostVolumeRootThatApprovesNoOwnedDirectoryIsRejected pins that a
// root must name one directory the test owns. Floci canonicalises each root and
// allows any source path starting with it, so the filesystem root — or anything
// that cleans to it — would approve every host mount, the very switch this
// helper does not offer. An empty or relative root names no directory at all.
func TestRunArgs_HostVolumeRootThatApprovesNoOwnedDirectoryIsRejected(t *testing.T) {
	for _, root := range []string{"/", "//", "/.", "/tmp/..", "", "relative/dir", "./dir"} {
		t.Run(root, func(t *testing.T) {
			args, err := render(WithHostVolumeRoots("/tmp/run-a", root))
			if err == nil {
				t.Fatalf("root %q was accepted: %q", root, args)
			}
		})
	}
}
