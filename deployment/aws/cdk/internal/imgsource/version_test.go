package imgsource

import (
	"fmt"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

func appBuildInfo(deps ...*debug.Module) *debug.BuildInfo {
	return &debug.BuildInfo{Main: debug.Module{Path: "example.com/app", Version: "(devel)"}, Deps: deps}
}

// Category: unit (TESTS.md §1).
func TestModuleVersion_ReturnsTheReleasedDependencyVersion(t *testing.T) {
	for _, version := range []string{"v0.4.0", "v1.2.3-rc.1"} {
		t.Run(version, func(t *testing.T) {
			got, err := moduleVersion(appBuildInfo(
				&debug.Module{Path: "github.com/aws/jsii-runtime-go", Version: "v1.139.0"},
				&debug.Module{Path: cdkModulePath, Version: version},
			), true)
			require.NoError(t, err)
			require.Equal(t, version, got)
		})
	}
}

// Category: unit (TESTS.md §1).
func TestModuleVersion_RejectsBuildsWithoutAPublishedCommand(t *testing.T) {
	cdk := func(version string) *debug.Module { return &debug.Module{Path: cdkModulePath, Version: version} }
	replaced := cdk("v0.4.0")
	replaced.Replace = &debug.Module{Path: "../gobridge/deployment/aws/cdk"}
	for name, tc := range map[string]struct {
		info *debug.BuildInfo
		ok   bool
	}{
		"no build information":     {info: nil, ok: false},
		"not a dependency":         {info: appBuildInfo(&debug.Module{Path: "example.com/other", Version: "v1.0.0"}), ok: true},
		"local replace":            {info: appBuildInfo(replaced), ok: true},
		"development build":        {info: appBuildInfo(cdk("(devel)")), ok: true},
		"pseudo-version":           {info: appBuildInfo(cdk("v0.0.0-20260911120000-abcdefabcdef")), ok: true},
		"pseudo-version after tag": {info: appBuildInfo(cdk("v0.4.1-0.20260911120000-abcdefabcdef")), ok: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := moduleVersion(tc.info, tc.ok)
			require.Error(t, err)
		})
	}
}

func BenchmarkModuleVersion_SmallApp(b *testing.B) {
	info := appBuildInfo(
		&debug.Module{Path: "github.com/aws/aws-cdk-go/awscdk/v2", Version: "v2.264.0"},
		&debug.Module{Path: cdkModulePath, Version: "v0.4.0"},
	)
	for b.Loop() {
		if _, err := moduleVersion(info, true); err != nil {
			b.Fatal(err)
		}
	}
}

// A large CDK app links hundreds of modules; the lookup is a linear scan with
// this module last, which is its worst case.
func BenchmarkModuleVersion_LargeApp(b *testing.B) {
	deps := make([]*debug.Module, 0, 501)
	for i := range 500 {
		deps = append(deps, &debug.Module{Path: fmt.Sprintf("example.com/dep%03d", i), Version: "v1.0.0"})
	}
	info := appBuildInfo(append(deps, &debug.Module{Path: cdkModulePath, Version: "v0.4.0"})...)
	for b.Loop() {
		if _, err := moduleVersion(info, true); err != nil {
			b.Fatal(err)
		}
	}
}
