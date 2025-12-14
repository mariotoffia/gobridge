package file

import "time"

// Config holds the configuration for the file-based ConfigSource.
type Config struct {
	// Path to config directory or single file.
	Path string `json:"path"`
	// WatchInterval for polling changes. If zero, fsnotify is used.
	// Set to a positive duration to use polling instead.
	WatchInterval time.Duration `json:"watchInterval,omitempty"`
	// Recursive scan subdirectories when Path is a directory.
	Recursive bool `json:"recursive,omitempty"`
}

// Option is a functional option for configuring the file ConfigSource.
type Option func(*Source)

// WithWatchInterval sets the polling interval for watching changes.
// If zero (default), fsnotify-based watching is used.
func WithWatchInterval(d time.Duration) Option {
	return func(s *Source) {
		s.config.WatchInterval = d
	}
}

// WithRecursive enables recursive directory scanning.
func WithRecursive(recursive bool) Option {
	return func(s *Source) {
		s.config.Recursive = recursive
	}
}
