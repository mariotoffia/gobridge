package sqs

import (
	"errors"
	"testing"

	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestMapError_KMSClassification is the regression for Finding 3. The
// substring fallback classified anything containing "AccessDenied" — including
// the SSE-KMS code "KmsAccessDenied" — as permanent ErrNotAuthorized, which
// false-DLQs every send during the 10-120s a freshly-granted KMS key policy /
// IAM role takes to propagate. Typed KMS checks must run BEFORE the string
// fallback and classify per AWS KMS semantics: transient codes recoverable,
// genuinely permanent codes not.
func TestMapError_KMSClassification(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		want        *shared.BridgeError
		recoverable bool
	}{
		{
			name:        "KmsAccessDenied is temporary (grant may still be propagating)",
			err:         &sqstypes.KmsAccessDenied{Message: strPtr("access denied")},
			want:        shared.ErrTemporaryAuthFailure,
			recoverable: true,
		},
		{
			name:        "KmsThrottled is throttling",
			err:         &sqstypes.KmsThrottled{Message: strPtr("slow down")},
			want:        shared.ErrThrottled,
			recoverable: true,
		},
		{
			name:        "KmsDisabled is permanent",
			err:         &sqstypes.KmsDisabled{Message: strPtr("key disabled")},
			want:        shared.ErrNotAuthorized,
			recoverable: false,
		},
		{
			name:        "KmsInvalidState is permanent",
			err:         &sqstypes.KmsInvalidState{Message: strPtr("pending deletion")},
			want:        shared.ErrNotAuthorized,
			recoverable: false,
		},
		{
			name:        "KmsNotFound is permanent",
			err:         &sqstypes.KmsNotFound{Message: strPtr("no such key")},
			want:        shared.ErrNotAuthorized,
			recoverable: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MapError(tc.err)
			if !errors.Is(got, tc.want) {
				t.Fatalf("MapError(%T) = %v, want class %v", tc.err, got, tc.want)
			}
			if r := shared.IsRecoverableError(got); r != tc.recoverable {
				t.Fatalf("IsRecoverableError(MapError(%T)) = %v, want %v", tc.err, r, tc.recoverable)
			}
		})
	}
}

// TestMapError_PlainAPIAuthStaysPermanent documents the deliberate residual of
// Finding 3: MapError is a stateless pure function, so a plain IAM/STS auth
// failure (no code-distinguishable KMS type) cannot implement a
// first-N-then-permanent scheme and is kept permanent. Only the typed KMS
// codes are re-classified.
func TestMapError_PlainAPIAuthStaysPermanent(t *testing.T) {
	for _, msg := range []string{"AccessDenied", "UnauthorizedAccess", "InvalidClientTokenId"} {
		got := MapError(errors.New(msg + ": denied"))
		if !errors.Is(got, shared.ErrNotAuthorized) {
			t.Fatalf("MapError(%q) = %v, want ErrNotAuthorized (stateless residual)", msg, got)
		}
	}
}
