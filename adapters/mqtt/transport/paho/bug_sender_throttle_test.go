package paho

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestBug_PublishReasonError_ThrottleHintOnly0x97 locks finding 7: the
// sender attaches a ThrottleRetryAfter back-off hint ONLY for PUBACK/PUBREC
// reason code 0x97 (Quota exceeded). The removed 0x93 / 0xA1 checks are not
// valid PUBACK/PUBREC codes and must classify as plain errors with no
// retry-after hint. Success codes map to nil.
func TestBug_PublishReasonError_ThrottleHintOnly0x97(t *testing.T) {
	s := &Sender{opts: SenderOptions{ThrottleRetryAfter: 750 * time.Millisecond}}

	cases := []struct {
		name         string
		code         byte
		wantNil      bool
		wantThrottle bool
		wantRetry    time.Duration
	}{
		{name: "success", code: 0x00, wantNil: true},
		{name: "no_matching_subscribers", code: 0x10, wantNil: true},
		{name: "quota_exceeded_0x97", code: 0x97, wantThrottle: true, wantRetry: 750 * time.Millisecond},
		{name: "dead_0x93_no_hint", code: 0x93},
		{name: "dead_0xA1_no_hint", code: 0xA1},
		{name: "not_authorized_0x87_no_hint", code: 0x87},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			berr := s.publishReasonError(tc.code, "t/x")
			if tc.wantNil {
				require.Nil(t, berr)
				return
			}
			require.NotNil(t, berr)
			if tc.wantThrottle {
				require.Equal(t, shared.ErrCodeThrottled, berr.Code, "0x97 must classify as throttled")
				require.Equal(t, tc.wantRetry, berr.RetryAfter, "0x97 must carry the throttle back-off hint")
			} else {
				require.Zero(t, berr.RetryAfter, "non-0x97 reason codes must carry NO retry-after hint")
			}
		})
	}
}

// TestBug_PublishReasonError_ThrottleHintDefault verifies the 0x97 hint
// falls back to 500ms when ThrottleRetryAfter is unset.
func TestBug_PublishReasonError_ThrottleHintDefault(t *testing.T) {
	s := &Sender{opts: SenderOptions{}}

	berr := s.publishReasonError(0x97, "t/x")
	require.NotNil(t, berr)
	require.Equal(t, 500*time.Millisecond, berr.RetryAfter)
}
