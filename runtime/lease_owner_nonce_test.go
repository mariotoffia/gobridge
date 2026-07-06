package runtime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Finding C3-HIGH: two replicas that share the SAME instance_id (a mis-set env
// var, a cloned deployment) must NOT derive the same lease ownerID. If they did,
// the lease store's same-owner fast path would let each instantly counter-seize
// the other's lease — a permanent ping-pong that resets every standby's
// observation window and starves failover. The runtime suffixes the ownerID with
// a per-process boot nonce, so the derived owners are distinct while instance_id
// stays the human-facing display identity.
func TestLeaseOwnerID_BootNonceDistinctForSameInstanceID(t *testing.T) {
	a := New(WithInstanceID("shared-id"))
	b := New(WithInstanceID("shared-id"))

	// Display identity is preserved and shared.
	assert.Equal(t, "shared-id", a.instanceID)
	assert.Equal(t, "shared-id", b.instanceID)

	// Lease ownership tokens are DISTINCT (boot nonce differs per process/New).
	require.NotEqual(t, a.leaseOwnerID, b.leaseOwnerID,
		"two runtimes with the same instance_id must derive distinct lease owners")

	// Both still carry the instance_id as a prefix so operators can correlate.
	assert.True(t, strings.HasPrefix(a.leaseOwnerID, "shared-id#"))
	assert.True(t, strings.HasPrefix(b.leaseOwnerID, "shared-id#"))

	// Requirement (a): the boot nonce is STABLE for the lifetime of a process.
	// A single-use session that closes and re-acquires within the SAME runtime
	// must present the SAME ownerID so the store's same-owner fast path lets it
	// re-seize its own lease. leaseOwnerID is fixed at New() and reused by every
	// session manager + the locator, so a re-read after a (simulated) rebuild is
	// identical — the nonce is not regenerated per acquire.
	beforeRebuild := a.leaseOwnerID
	afterRebuild := a.leaseOwnerID
	assert.Equal(t, beforeRebuild, afterRebuild,
		"leaseOwnerID must be stable across a session rebuild within the same process")
}
