package gobridgebase

import (
	"fmt"
	"os"
	"path/filepath"
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
	})
	return seederValue, seederErr
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
