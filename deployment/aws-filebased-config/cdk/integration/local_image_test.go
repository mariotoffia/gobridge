//go:build integration_local

package integration

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalRuntimeImage_ReplacesOnlyBridgeImage(t *testing.T) {
	for _, tc := range []struct {
		name, override, want string
	}{
		{name: "default", want: localImage},
		{name: "override", override: " example.com/custom:local ", want: "example.com/custom:local"},
		{name: "blank override", override: "  ", want: localImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(localImageEnv, tc.override)
			main := map[string]any{"Name": "gobridge", "Image": map[string]any{"Fn::Join": []any{}}}
			sidecar := map[string]any{"Name": "other", "Image": "other:tag"}
			props := map[string]any{"ContainerDefinitions": []any{main, sidecar}}
			useLocalRuntimeImage(props)
			require.Equal(t, tc.want, main["Image"])
			require.Equal(t, "other:tag", sidecar["Image"])
		})
	}
}
