package bridge

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRecordedDigestMatches_KeepsTheFirstAssociationForALegacyDigest pins the
// one thing a lossy digest must not be allowed to do: vouch for two different
// configurations in turn. Two documents that differ only in an integer above
// 2^53 share one legacy digest. Once that digest has been associated with the
// first document's identity, the second must fail closed, not overwrite the
// association and take the first one's place as "the committed content".
func TestRecordedDigestMatches_KeepsTheFirstAssociationForALegacyDigest(t *testing.T) {
	first := maxBytesConfig(3, wideInteger)
	second := maxBytesConfig(3, wideInteger-1)
	raw, ok := legacyConfigCanonicalBytes(first)
	require.True(t, ok)
	legacy := candidateConfigDigest(raw)
	rawSecond, ok := legacyConfigCanonicalBytes(second)
	require.True(t, ok)
	require.Equal(t, legacy, candidateConfigDigest(rawSecond), "the two must collide under the legacy projection")

	b := &rolloutBarrier{}
	require.True(t, b.recordedDigestMatches(first, legacy, 3), "an unseen legacy digest is matched by the lossy projection once")
	require.False(t, b.recordedDigestMatches(second, legacy, 3), "a different identity must fail closed once the digest is associated")
	require.True(t, b.recordedDigestMatches(first, legacy, 3), "the first association stands")
}
