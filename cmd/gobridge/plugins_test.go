package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tagged tests declare their presence independently of production family wiring,
// so an accidental untagged compiledFamilies append cannot skip the blank test.
var expectedFamilies []string

func requireBlankBuild(t *testing.T) {
	t.Helper()
	if len(expectedFamilies) != 0 {
		t.Skip("blank-root assertion requires a build without plugin families")
	}
}

// TestBlankRoot_RegistersNoKinds verifies the untagged aggregates are no-ops.
func TestBlankRoot_RegistersNoKinds(t *testing.T) {
	requireBlankBuild(t)
	reg := ports.NewRegistry()
	if err := registerAllDecoders(reg); err != nil {
		t.Fatal(err)
	}
	if got := reg.Kinds(); len(got) != 0 {
		t.Fatalf("blank root registered %v", got)
	}
	if len(compiledFamilies) != 0 {
		t.Fatalf("blank root compiled families %v", compiledFamilies)
	}
	if err := wireAllFactories(t.Context(), bridge.NewSupervisor(), discardLogger(), nil); err != nil {
		t.Fatal(err)
	}
	if err := seedAllStores(context.Background(), bridge.NewBuilder(&ports.BridgeConfig{})); err != nil {
		t.Fatal(err)
	}
}

// TestBlankRoot_NilObservability verifies the blank root creates no exporters.
func TestBlankRoot_NilObservability(t *testing.T) {
	requireBlankBuild(t)
	metrics, closeMetrics, err := newMetricsExporter(t.Context(), discardLogger())
	require.NoError(t, err)
	assert.Nil(t, metrics)
	assert.Nil(t, closeMetrics)

	tracer, closeTracer, err := newTracer(t.Context(), discardLogger())
	require.NoError(t, err)
	assert.Nil(t, tracer)
	assert.Nil(t, closeTracer)
}

// TestPluginSummary_NoFamilies verifies a blank root reports an empty list.
func TestPluginSummary_NoFamilies(t *testing.T) {
	original := compiledFamilies
	t.Cleanup(func() { compiledFamilies = original })
	compiledFamilies = nil

	assert.Equal(t, "families=[]", pluginSummary())
}

// TestPluginSummary_SortsFamilies verifies reporting leaves build metadata unchanged.
func TestPluginSummary_SortsFamilies(t *testing.T) {
	original := compiledFamilies
	t.Cleanup(func() { compiledFamilies = original })
	compiledFamilies = []string{"native", "mqtt"}

	assert.Equal(t, "families=[mqtt native]", pluginSummary())
	assert.Equal(t, []string{"native", "mqtt"}, compiledFamilies)
}

// TestVersionLine_UnstampedIsDev verifies both absent linker stamps render as dev.
func TestVersionLine_UnstampedIsDev(t *testing.T) {
	oldVersion, oldSHA, oldFamilies := version, gitSHA, compiledFamilies
	t.Cleanup(func() { version, gitSHA, compiledFamilies = oldVersion, oldSHA, oldFamilies })
	version, gitSHA, compiledFamilies = "", "", nil

	assert.Equal(t, "gobridge dev (dev) families=[]", versionLine())
}

// TestVersionLine_Stamped verifies linker metadata and sorted families are preserved.
func TestVersionLine_Stamped(t *testing.T) {
	oldVersion, oldSHA, oldFamilies := version, gitSHA, compiledFamilies
	t.Cleanup(func() { version, gitSHA, compiledFamilies = oldVersion, oldSHA, oldFamilies })
	version, gitSHA, compiledFamilies = "v1.2.3", "abc123", []string{"native", "mqtt"}

	assert.Equal(t, "gobridge v1.2.3 (abc123) families=[mqtt native]", versionLine())
}

// TestStartupLog_WarnsWhenNothingLinked verifies the blank-root warning is actionable.
func TestStartupLog_WarnsWhenNothingLinked(t *testing.T) {
	original := compiledFamilies
	t.Cleanup(func() { compiledFamilies = original })
	compiledFamilies = nil
	var buf bytes.Buffer

	logStartup(captureLogger(&buf), ports.NewRegistry())

	assert.Contains(t, buf.String(), "families=[]")
	assert.Contains(t, buf.String(), "kinds=[]")
	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "no transports or stores linked")
	assert.Contains(t, buf.String(), "every routed config will be rejected")
	assert.Contains(t, buf.String(), "-tags gobridge_")
}

// TestStartupLog_ReportsLinkedCapabilities verifies either kind of metadata avoids the blank warning.
func TestStartupLog_ReportsLinkedCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, summary, kindsText string
		families, kinds          []string
	}{
		{"families only", "families=[otel]", "kinds=[]", []string{"otel"}, nil},
		{"kinds only", "families=[]", `kinds="[alpha zeta]"`, nil, []string{"zeta", "alpha"}},
		{"both", "families=[mqtt native]", `kinds="[alpha zeta]"`, []string{"native", "mqtt"}, []string{"zeta", "alpha"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := compiledFamilies
			t.Cleanup(func() { compiledFamilies = original })
			compiledFamilies = tc.families
			reg := ports.NewRegistry()
			for _, kind := range tc.kinds {
				require.NoError(t, reg.Register(kind, func(ports.RawConfig) (ports.PluginConfig, error) {
					t.Fatal("startup reporting must not invoke a decoder")
					return nil, nil
				}))
			}
			var buf bytes.Buffer

			logStartup(captureLogger(&buf), reg)

			assert.Contains(t, buf.String(), "level=INFO")
			assert.Contains(t, buf.String(), tc.summary)
			assert.Contains(t, buf.String(), tc.kindsText)
			assert.NotContains(t, buf.String(), "level=WARN")
		})
	}
}

// TestPluginSummary_CommandFlags verifies reporting exits before loading configuration.
func TestPluginSummary_CommandFlags(t *testing.T) {
	for _, arg := range []string{"-version", "-help"} {
		t.Run(arg, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestPluginSummary_CommandProcess$")
			cmd.Env = append(os.Environ(), "GOBRIDGE_TEST_REPORT_FLAG="+arg,
				"GOBRIDGE_TEST_CONFIG="+filepath.Join(t.TempDir(), "missing.yaml"))
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			require.NoError(t, cmd.Run(), "stdout: %s; stderr: %s", &stdout, &stderr)
			if arg == "-version" {
				assert.Equal(t, versionLine()+"\n", stdout.String())
				assert.Empty(t, stderr.String())
			} else {
				assert.Empty(t, stdout.String())
				assert.Contains(t, stderr.String(), pluginSummary())
				assert.Contains(t, stderr.String(), "-version")
				assert.Contains(t, stderr.String(), "-tags gobridge_")
				assert.Contains(t, stderr.String(), "PLUGIN.md")
				assert.NotContains(t, stderr.String(), "Links the MQTT transport")
			}
		})
	}
}

// TestPluginSummary_CommandProcess runs reporting flags with isolated process state.
func TestPluginSummary_CommandProcess(t *testing.T) {
	arg := os.Getenv("GOBRIDGE_TEST_REPORT_FLAG")
	if arg == "" {
		return
	}
	os.Args = []string{"gobridge", "-config", os.Getenv("GOBRIDGE_TEST_CONFIG"), "-start-empty=false", arg}
	flag.CommandLine = flag.NewFlagSet("gobridge", flag.ExitOnError)
	flag.CommandLine.Usage = func() { flag.Usage() }
	os.Exit(run())
}
