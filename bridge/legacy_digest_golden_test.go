package bridge

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The legacy projection has to reproduce bytes that were hashed years ago, and
// this is what stops it from being quietly improved.
//
// Releases up to v0.4.1 recorded their rollout-row and committed-config digests
// over a tree decoded into an `any`, where every JSON number is a float64. An
// integer wider than 2^53 — an int64 option such as a transport's maximum body
// size — was therefore ROUNDED before it was hashed. Those records are still out
// there in upgraded cohorts. Recomputing them any other way rejects a member
// that is in fact running the committed configuration, so the rounding is part
// of the contract rather than a defect to repair.

// wideInteger is the first integer a float64 cannot hold: it rounds down to
// 9007199254740992, the value one below it.
const wideInteger = int64(9007199254740993)

// wideIntegerLegacyDigest is the digest a release up to v0.4.1 recorded for
// maxBytesConfig(3, wideInteger). It is written out here rather than computed,
// so the test cannot drift along with the code it checks: whatever the
// implementation and the copy below do, this value is what the fleet's older
// records actually carry.
const wideIntegerLegacyDigest = "9ed90e0f6c0d5dff50168cad3d8ecf2dda6d3f4681929c7252060a6ab1330953"

// maxBytesConfig is a fixed single-session document whose plugin carries one
// int64 option. Version 3 with wideInteger is the fixture the golden digest
// above was taken over, so neither may change without recomputing it.
func maxBytesConfig(version int, maxBytes int64) *ports.BridgeConfig {
	session := ports.SessionDef{ID: "s", Transport: "roundtrip"}
	session.SetDecoded(&roundTripConfig{ClientID: "c", KeepAlive: 30, MaxBytes: maxBytes}, nil)
	return &ports.BridgeConfig{
		Version:  version,
		Bridge:   ports.BridgeSettings{ID: "demo", DeploymentMode: "clustered"},
		Sessions: []ports.SessionDef{session},
	}
}

// historicalProjection is a copy of the projection those releases wrote, kept
// local to the test so it cannot be refactored together with the production
// code: marshal, read the bytes back into an `any` with float64 numbers, drop
// the empty collections, marshal again, newline — for the document as written
// and then for each decoded plugin config in traversal order.
func historicalProjection(t *testing.T, cfg *ports.BridgeConfig) []byte {
	t.Helper()
	var buf bytes.Buffer
	historicalEncode(t, &buf, shared.RevealSecrets(cfg))
	visitPluginConfigs(cfg, func(pc ports.PluginConfig) {
		historicalEncode(t, &buf, shared.RevealSecrets(pc))
	})
	return buf.Bytes()
}

func historicalEncode(t *testing.T, buf *bytes.Buffer, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	var tree any
	require.NoError(t, json.Unmarshal(raw, &tree))
	normalized, keep := ports.WithoutEmptyCollections(tree)
	if !keep {
		buf.WriteString("null\n")
		return
	}
	out, err := json.Marshal(normalized)
	require.NoError(t, err)
	buf.Write(out)
	buf.WriteByte('\n')
}

func TestLegacyDigest_ReproducesTheHistoricalProjectionForWideIntegers(t *testing.T) {
	cfg := maxBytesConfig(3, wideInteger)

	historical := historicalProjection(t, cfg)
	require.Equal(t, wideIntegerLegacyDigest, candidateConfigDigest(historical),
		"the golden digest is what the historical algorithm produces for this fixture; "+
			"a failure here means the copy above drifted, not the implementation")

	legacy, ok := legacyConfigCanonicalBytes(cfg)
	require.True(t, ok)
	assert.Equal(t, string(historical), string(legacy),
		"the legacy projection must hand back the bytes the old release hashed, rounding and all")
	assert.Equal(t, wideIntegerLegacyDigest, candidateConfigDigest(legacy),
		"a member that computes anything else stops recognising every digest recorded before "+
			"the content normal form")

	normalForm, ok := configCanonicalBytes(cfg)
	require.True(t, ok)
	assert.NotEqual(t, candidateConfigDigest(normalForm), candidateConfigDigest(legacy),
		"the current identity keeps the digits the option was written with, so the two spellings "+
			"of this document differ")

	// The same fixture with an option a float64 holds exactly, both encoders run
	// over the same normalised document: they agree there, so what separates the
	// two spellings above is the width of the number and not a projection that
	// differs everywhere.
	narrow := maxBytesConfig(0, 1<<20)
	narrowLegacy, ok := legacyConfigCanonicalBytes(ports.ContentNormalForm(narrow))
	require.True(t, ok)
	narrowNormalForm, ok := configCanonicalBytes(narrow)
	require.True(t, ok)
	assert.Equal(t, string(narrowNormalForm), string(narrowLegacy))
}

func TestRecordedDigestMatches_AcceptsALegacyDigestWithWideIntegers(t *testing.T) {
	cfg := maxBytesConfig(3, wideInteger)
	recorded := candidateConfigDigest(historicalProjection(t, cfg))

	assert.True(t, (&rolloutBarrier{}).recordedDigestMatches(cfg, recorded, cfg.Version),
		"a rollout row or committed artifact written before the content normal form, over a "+
			"document carrying an option wider than a float64, still names this configuration")
}
