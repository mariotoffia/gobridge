package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeParameterRef(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{
			name:    "empty string returns error",
			input:   "",
			wantErr: "empty parameter reference",
		},
		{
			name:  "absolute path returned unchanged",
			input: "/gobridge/admin",
			want:  "/gobridge/admin",
		},
		{
			name:  "relative path gets leading slash",
			input: "gobridge/admin",
			want:  "/gobridge/admin",
		},
		{
			name:  "pms URL with host and path",
			input: "pms://gobridge/admin/key",
			want:  "/gobridge/admin/key",
		},
		{
			name:  "pms URL with host only",
			input: "pms://gobridge",
			want:  "/gobridge",
		},
		{
			name:  "pms URL with trailing slash is trimmed",
			input: "pms://gobridge/",
			want:  "/gobridge",
		},
		{
			name:    "pms URL with empty host returns error",
			input:   "pms://",
			wantErr: "requires authority or absolute-path form",
		},
		{
			name:    "authority URI trailing whitespace is rejected",
			input:   "pms://gobridge/admin ",
			wantErr: "invalid parameter reference",
		},
		{
			name:    "authority URI leading whitespace is rejected",
			input:   " pms://gobridge/admin",
			wantErr: "invalid parameter reference",
		},
		{
			name:    "absolute URI trailing whitespace is rejected",
			input:   "pms:///gobridge/admin\t",
			wantErr: "invalid parameter reference",
		},
		{
			name:    "absolute URI leading whitespace is rejected",
			input:   "\npms:///gobridge/admin",
			wantErr: "invalid parameter reference",
		},
		{
			name:  "absolute plain path trims boundary whitespace",
			input: " /gobridge/admin ",
			want:  "/gobridge/admin",
		},
		{
			name:  "relative plain path trims boundary whitespace",
			input: "\tgobridge/admin\n",
			want:  "/gobridge/admin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeParameterRef(tc.input)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
