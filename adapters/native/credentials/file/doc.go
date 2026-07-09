// Package file provides a file-based credentials repository.
//
// Credentials are stored as JSON files in a directory hierarchy.
// Each credential is written with mode 0600 and versioned with
// optimistic locking for concurrent access safety.
//
// URI format: file://namespace/path/to/creds
// Maps to:    basePath/namespace/path/to/creds.json
//
// Every load and every write requires a credential set to carry at least one
// usable credential — a non-empty username or password, a CA bundle, or a
// complete cert+key pair (whitespace-only material counts as absent, and a
// torn cert/key half is rejected outright). An empty set can therefore neither
// be read back as a valid anonymous credential nor silently replace live
// credentials on rotation. URI parse errors are redacted so an embedded
// `user:pass@` credential can never leak into a log or error.
package file
