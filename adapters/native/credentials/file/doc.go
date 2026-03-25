// Package file provides a file-based credentials repository.
//
// Credentials are stored as JSON files in a directory hierarchy.
// Each credential is written with mode 0600 and versioned with
// optimistic locking for concurrent access safety.
//
// URI format: file://namespace/path/to/creds
// Maps to:    basePath/namespace/path/to/creds.json
package file
