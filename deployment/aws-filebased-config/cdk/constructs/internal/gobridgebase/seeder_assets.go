package gobridgebase

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// seederAssets are the seeder shell + pinned image reference loaded
// lazily from the canonical sibling seeder/ directory at synth time.
//
// Embedding via //go:embed is not used because the source files live
// outside this package (in constructs/internal/seeder/) and the embed
// directive cannot reach across package boundaries. CDK constructs
// only run during synth from a checkout, so a runtime.Caller-based
// lookup is safe and avoids duplicating files.
type seederAssets struct {
	script string
	image  string
}

var (
	seederOnce  sync.Once
	seederValue seederAssets
	seederErr   error
)

func loadSeederAssets() (seederAssets, error) {
	seederOnce.Do(func() {
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			seederErr = fmt.Errorf("gobridgebase: unable to resolve runtime.Caller for seeder asset lookup")
			return
		}
		seederDir := filepath.Join(filepath.Dir(thisFile), "..", "seeder")
		scriptBytes, err := os.ReadFile(filepath.Join(seederDir, "seeder.sh"))
		if err != nil {
			seederErr = fmt.Errorf("gobridgebase: read seeder.sh: %w", err)
			return
		}
		imageBytes, err := os.ReadFile(filepath.Join(seederDir, "image.txt"))
		if err != nil {
			seederErr = fmt.Errorf("gobridgebase: read image.txt: %w", err)
			return
		}
		seederValue = seederAssets{
			script: string(scriptBytes),
			image:  strings.TrimSpace(string(imageBytes)),
		}
		if err := validateSeederImageRef(seederValue.image); err != nil {
			seederErr = err
			return
		}
	})
	return seederValue, seederErr
}

// seederImageRefPattern matches a fully-pinned "<repo>:<tag>@sha256:<64hex>"
// reference. The digest MUST be a real 64-hex sha256 — an all-zeros
// placeholder (the pre-provisioning sentinel) is rejected explicitly by
// validateSeederImageRef so a checkout that forgot `make update-seeder-image`
// fails fast at synth instead of deploying a task that cannot pull its
// seeder and therefore never starts the main container.
var seederImageRefPattern = regexp.MustCompile(`^[^@\s]+@sha256:([0-9a-f]{64})$`)

// zeroSHA256 is the all-zeros placeholder digest shipped in image.txt before
// the first real digest is pinned. It is never a valid pullable digest.
const zeroSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"

// validateSeederImageRef rejects an unpinned or placeholder seeder image
// reference at synth time. The message points operators at the fix.
func validateSeederImageRef(ref string) error {
	m := seederImageRefPattern.FindStringSubmatch(ref)
	if m == nil {
		return fmt.Errorf(
			"gobridgebase: seeder image reference %q is not a fully-pinned "+
				"\"<repo>:<tag>@sha256:<digest>\" value; run `make -C deployment/aws-filebased-config update-seeder-image` "+
				"and commit constructs/internal/seeder/image.txt",
			ref,
		)
	}
	if m[1] == zeroSHA256 {
		return fmt.Errorf(
			"gobridgebase: seeder image digest is the all-zeros placeholder "+
				"(%q); the seeder container cannot be pulled and the main container "+
				"depends on its SUCCESS, so every task would hang on startup. Run "+
				"`make -C deployment/aws-filebased-config update-seeder-image` and commit the real digest",
			ref,
		)
	}
	return nil
}

// DefaultSeederImage returns the pinned aws-cli image reference
// shipped with constructs/internal/seeder/image.txt. Format:
// "<repo>:<tag>@sha256:<digest>".
func DefaultSeederImage() string {
	a, err := loadSeederAssets()
	if err != nil {
		panic(err.Error())
	}
	return a.image
}

// SeederScript returns the canonical seeder shell script bytes.
func SeederScript() string {
	a, err := loadSeederAssets()
	if err != nil {
		panic(err.Error())
	}
	return a.script
}
