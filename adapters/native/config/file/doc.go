// Package file implements config.Loader and config.Watcher for file-based
// configuration sources.
//
// Source wraps config.ParseFile to load a BridgeConfig from a YAML or JSON
// file on disk.
//
// Watcher detects file changes using one of two modes:
//   - "notify" (default): filesystem event notifications via fsnotify,
//     debounced to coalesce rapid writes
//   - "poll": periodic file reads with SHA-256 content comparison,
//     suitable for network mounts where fsnotify is unreliable
//
// Watch mode and intervals can be configured programmatically or via
// config.ConfigWatchDef from the YAML config itself.
package file
