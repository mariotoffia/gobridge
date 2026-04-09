package amqp091

import "testing"

// verifies injectCredentials merges user-info into a broker URL.
func TestInjectCredentials(t *testing.T) {
	tests := []struct {
		name     string
		broker   string
		user     string
		pass     string
		want     string
	}{
		{
			name:   "empty username returns original",
			broker: "amqp://localhost:5672/",
			user:   "",
			pass:   "",
			want:   "amqp://localhost:5672/",
		},
		{
			name:   "valid URL gets credentials injected",
			broker: "amqp://localhost:5672/",
			user:   "guest",
			pass:   "secret",
			want:   "amqp://guest:secret@localhost:5672/",
		},
		{
			name:   "existing user-info is preserved",
			broker: "amqp://alice:pw@localhost:5672/",
			user:   "bob",
			pass:   "other",
			want:   "amqp://alice:pw@localhost:5672/",
		},
		{
			name:   "invalid URL returns original",
			broker: "://bad url",
			user:   "u",
			pass:   "p",
			want:   "://bad url",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := injectCredentials(tt.broker, tt.user, tt.pass)
			if got != tt.want {
				t.Errorf("injectCredentials(%q, %q, %q) = %q, want %q",
					tt.broker, tt.user, tt.pass, got, tt.want)
			}
		})
	}
}

// verifies redactURL strips credentials or flags invalid URLs.
func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "URL with credentials is redacted",
			raw:  "amqp://guest:secret@localhost:5672/",
			want: "amqp://REDACTED@localhost:5672/",
		},
		{
			name: "URL without credentials is unchanged",
			raw:  "amqp://localhost:5672/",
			want: "amqp://localhost:5672/",
		},
		{
			name: "invalid URL returns sentinel",
			raw:  "://bad url",
			want: "<invalid-url>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactURL(tt.raw)
			if got != tt.want {
				t.Errorf("redactURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
