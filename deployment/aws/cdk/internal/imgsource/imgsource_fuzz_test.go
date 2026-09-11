package imgsource

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func FuzzGoBuild_DockerfileInputs(f *testing.F) {
	for field, value := range []string{
		"v1.2.3", "example.com/bridge/cmd/custom", "custom", defaultGoImage, defaultBaseImage, "linux/arm64",
	} {
		f.Add(uint8(field), value)
		f.Add(uint8(field), "\nRUN false")
		f.Add(uint8(field), "$(false)")
	}
	f.Add(uint8(0), "v1.2.3-rc..1")
	f.Add(uint8(0), "v1.2.3-01")
	f.Fuzz(func(t *testing.T, field uint8, value string) {
		props := GoBuildProps{Version: "v1.2.3"}
		switch field % 6 {
		case 0:
			props.Version = value
		case 1:
			props.Package = value
		case 2:
			props.BuildTags = []string{value}
		case 3:
			props.GoImage = value
		case 4:
			props.BaseImage = value
		case 5:
			props.Platform = value
		}
		var dockerfile string
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					message, ok := recovered.(string)
					require.True(t, ok, "invalid input must produce a synth error, not a runtime panic")
					require.Contains(t, message, "gobridgecdk: ImageFromGoBuild")
				}
			}()
			dockerfile = renderDockerfile(props, nil)
		}()
		if dockerfile == "" {
			return // Rejected at synth, before producing a Docker build context.
		}
		require.Equal(t, 2, strings.Count(dockerfile, "FROM "))
		require.Equal(t, 1, strings.Count(dockerfile, "\nRUN "))
		require.Equal(t, 1, strings.Count(dockerfile, "\nENTRYPOINT "))
		require.NotContains(t, dockerfile, "\r")
		require.NotContains(t, dockerfile, "$")
		require.NotContains(t, dockerfile, "`")
		require.NotContains(t, dockerfile, ";")
	})
}
