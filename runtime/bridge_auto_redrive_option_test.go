package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNew_AutoRedriveWindow(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
		want time.Duration
	}{
		// A library user who never names the option gets automatic redrive on.
		{"default is a day", nil, 24 * time.Hour},
		{"option sets the window", []Option{WithAutoRedriveWindow(time.Hour)}, time.Hour},
		{"zero turns it off", []Option{WithAutoRedriveWindow(0)}, 0},
		{"negative turns it off", []Option{WithAutoRedriveWindow(-time.Minute)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := New(tc.opts...)
			assert.Equal(t, tc.want, rt.autoRedrive.window)
		})
	}
}
