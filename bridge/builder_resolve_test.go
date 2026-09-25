package bridge

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// replicaIdentityFreezer advertises a replica identity strategy but freezes to
// a plain freezeOutputConfig, which has none. The copy keeps its kind and still
// validates, so only the capability check notices.
type replicaIdentityFreezer struct{ freezeOutputConfig }

func (replicaIdentityFreezer) ReplicaIdentityStrategy() string { return ports.ReplicaIdentityHostname }

// TestFreezePluginConfig_RejectsAFrozenCopyThatDroppedReplicaIdentity pins the
// replica identity strategy as a capability the builder's freeze must keep. The
// validator reads it off the frozen copy: without it, and with no $share/ topic,
// the clustered-receiver rule is skipped, so a receiver that gets every message
// once per replica passes validation.
func TestFreezePluginConfig_RejectsAFrozenCopyThatDroppedReplicaIdentity(t *testing.T) {
	source := replicaIdentityFreezer{freezeOutputConfig{kind: "mqtt"}}

	_, err := freezePluginConfig(source)

	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

var _ ports.ReplicaIdentityConfig = replicaIdentityFreezer{}
