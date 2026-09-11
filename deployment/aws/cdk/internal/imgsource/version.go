package imgsource

import (
	"errors"
	"fmt"
	"regexp"
	"runtime/debug"
)

// cdkModulePath is this module. Every published module in the repository shares
// one version, so the version the running CDK app depends on is also the version
// of the command the image builds.
const cdkModulePath = "github.com/mariotoffia/gobridge/deployment/aws/cdk"

// A pseudo-version names a commit, not a release, so no published command
// carries it.
var pseudoVersion = regexp.MustCompile(`[.-][0-9]{14}-[0-9a-f]{12}$`)

// moduleVersion returns the released version of this module that the running
// program was built with. A development build, a local replace or a
// pseudo-version has no matching published command, so it is an error and the
// caller must set Version instead.
func moduleVersion(info *debug.BuildInfo, ok bool) (string, error) {
	if !ok || info == nil {
		return "", errors.New("the program carries no module build information")
	}
	for _, dep := range info.Deps {
		if dep.Path != cdkModulePath {
			continue
		}
		if dep.Replace != nil {
			return "", fmt.Errorf("%s is replaced by %s", cdkModulePath, dep.Replace.Path)
		}
		if !versionTag.MatchString(dep.Version) || pseudoVersion.MatchString(dep.Version) {
			return "", fmt.Errorf("%s is not a released version (%s)", cdkModulePath, dep.Version)
		}
		return dep.Version, nil
	}
	return "", fmt.Errorf("%s is not a dependency of this program", cdkModulePath)
}
