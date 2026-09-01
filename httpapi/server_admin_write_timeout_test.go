package httpapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// txnEnabledServer builds a Server with the config-transaction endpoints
// enabled, which is what makes the slow commit response path reachable.
func txnEnabledServer(t *testing.T, adminOpTimeout time.Duration) *Server {
	t.Helper()
	store := &guardTestStore{disk: sampleBridgeConfig()}
	s := New(nil, Config{
		AdminAddr:             ":0",
		MonitorAddr:           ":0",
		AdminAPIKey:           shared.NewSecret("test-admin-key-1234567890"),
		AdminOperationTimeout: adminOpTimeout,
		ConfigStore:           store,
		ConfigProvider:        func() *ports.BridgeConfig { return nil },
	}, WithServerLogger(nil))
	require.NotNil(t, s.configTxn, "precondition: the config transaction endpoints must be enabled")
	return s
}

// TestAdminWriteTimeout_CoversWorstCaseCommitResponse proves the admin
// listener's WriteTimeout is derived from the LONGEST response path it can
// serve, not from AdminOperationTimeout alone. A config commit answers only
// after the detached apply AND, when the apply fails, the rollback restore have
// both run to their own deadlines. A shorter write deadline resets the
// connection while that work is still legitimately running, so automation sees
// a transport error for a request that is about to succeed (or to report
// rolled_back) and retries against an ambiguous state.
func TestAdminWriteTimeout_CoversWorstCaseCommitResponse(t *testing.T) {
	s := txnEnabledServer(t, 30*time.Second)

	got := s.adminWriteTimeout()

	worstCase := commitApplyTimeout + commitApplyTimeout
	assert.GreaterOrEqual(t, got, worstCase+adminWriteTimeoutMargin,
		"the admin WriteTimeout must cover a commit whose apply times out and whose rollback restore then also times out, plus the response flush margin")
}

// TestAdminWriteTimeout_HonorsALongerAdminOperationTimeout proves the derivation
// is a maximum over every response path: an operator who raises
// AdminOperationTimeout beyond the commit path still gets a write deadline that
// outlives a legitimately slow start/stop.
func TestAdminWriteTimeout_HonorsALongerAdminOperationTimeout(t *testing.T) {
	const opTimeout = 10 * time.Minute
	s := txnEnabledServer(t, opTimeout)

	assert.GreaterOrEqual(t, s.adminWriteTimeout(), opTimeout+adminWriteTimeoutMargin,
		"a raised AdminOperationTimeout must still dominate the derived write deadline")
}

// TestAdminWriteTimeout_WithoutConfigTransactionsStaysTight proves the long
// commit path does not loosen the write deadline for a server that cannot serve
// it: with no ConfigStore wired there is no commit endpoint, so the deadline
// stays at AdminOperationTimeout plus the flush margin and keeps its
// slow-client protection.
func TestAdminWriteTimeout_WithoutConfigTransactionsStaysTight(t *testing.T) {
	s := New(nil, Config{
		AdminAddr:             ":0",
		MonitorAddr:           ":0",
		AdminAPIKey:           shared.NewSecret("test-admin-key-1234567890"),
		AdminOperationTimeout: 30 * time.Second,
	}, WithServerLogger(nil))
	require.Nil(t, s.configTxn, "precondition: the config transaction endpoints must be disabled")

	assert.Equal(t, 30*time.Second+adminWriteTimeoutMargin, s.adminWriteTimeout(),
		"without a commit endpoint the admin write deadline must not be stretched to the commit worst case")
}
