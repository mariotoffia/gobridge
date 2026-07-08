package shared

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// secretTok is embedded as URI userinfo across these cases; RedactURI and
// RedactURIError must guarantee it never survives into a log/error string. It
// is concatenated into URIs at runtime so the sensitive `scheme://user:tok@`
// literal never appears in source.
const secretTok = "s3cr3t"

func TestRedactURI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		want    string
		absent  string // substring that must NOT appear in the result
		present string // substring that MUST remain (resource identity)
	}{
		{
			name:    "strips userinfo",
			raw:     "pms://user:" + secretTok + "@ns/param",
			want:    "pms://ns/param",
			absent:  secretTok,
			present: "pms://ns/param",
		},
		{
			name:    "strips query and fragment",
			raw:     "pms://user:" + secretTok + "@ns/param?token=" + secretTok + "#f",
			want:    "pms://ns/param",
			absent:  secretTok,
			present: "pms://ns/param",
		},
		{
			name:    "no userinfo is unchanged",
			raw:     "pms://ns/param",
			want:    "pms://ns/param",
			present: "pms://ns/param",
		},
		{
			name:    "file scheme unchanged",
			raw:     "file://broker/mqtt",
			want:    "file://broker/mqtt",
			present: "broker/mqtt",
		},
		{
			name: "empty stays empty",
			raw:  "",
			want: "",
		},
		{
			name:    "unparseable uri with userinfo is lexically redacted",
			raw:     "pms://user:" + secretTok + "@ns/\x7fbad",
			absent:  secretTok,
			present: "pms://ns/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RedactURI(tc.raw)
			if tc.want != "" && got != tc.want {
				t.Errorf("RedactURI(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Errorf("RedactURI(%q) = %q, must not contain %q", tc.raw, got, tc.absent)
			}
			if tc.present != "" && !strings.Contains(got, tc.present) {
				t.Errorf("RedactURI(%q) = %q, must retain resource identity %q", tc.raw, got, tc.present)
			}
		})
	}
}

func TestRedactURIError(t *testing.T) {
	t.Parallel()

	t.Run("nil passes through", func(t *testing.T) {
		t.Parallel()
		if got := RedactURIError(nil); got != nil {
			t.Fatalf("RedactURIError(nil) = %v, want nil", got)
		}
	})

	t.Run("non url.Error is unchanged", func(t *testing.T) {
		t.Parallel()
		base := errors.New("plain error")
		if got := RedactURIError(base); got != base {
			t.Fatalf("RedactURIError(plain) = %v, want the same error", got)
		}
	})

	t.Run("url.Error userinfo is redacted", func(t *testing.T) {
		t.Parallel()
		ue := &url.Error{
			Op:  "parse",
			URL: "pms://user:" + secretTok + "@ns/param",
			Err: errors.New("boom"),
		}
		got := RedactURIError(ue)
		if strings.Contains(got.Error(), secretTok) {
			t.Fatalf("RedactURIError error string %q must not contain the secret", got.Error())
		}
		// The wrapped cause must be preserved for errors.Is/As chains.
		if !errors.Is(got, ue.Err) {
			t.Fatalf("RedactURIError must preserve the wrapped cause")
		}
	})

	t.Run("wrapped url.Error is redacted (no raw URL leaks through a wrapper)", func(t *testing.T) {
		t.Parallel()
		// A raw *url.Error wrapped by fmt.Errorf bakes the raw URL into the outer
		// message at construction time. RedactURIError must still guarantee the
		// secret never survives — it extracts and redacts the INNER *url.Error
		// rather than trusting the baked outer string. (Mutating the inner in
		// place would NOT help: the outer message is already formatted, so it
		// would leak. This test guards against that regression.)
		inner := &url.Error{
			Op:  "parse",
			URL: "pms://user:" + secretTok + "@ns/param",
			Err: errors.New("boom"),
		}
		wrapped := fmt.Errorf("credential resolver: invalid URI: %w", inner)
		got := RedactURIError(wrapped)
		if strings.Contains(got.Error(), secretTok) {
			t.Fatalf("wrapped url.Error must not leak the secret: %q", got.Error())
		}
		if !errors.Is(got, inner.Err) {
			t.Fatalf("underlying cause must remain reachable via errors.Is")
		}
	})
}
