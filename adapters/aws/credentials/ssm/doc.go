// Package ssm provides an AWS Systems Manager Parameter Store credentials
// repository implementing ports.CredentialRepository and ports.CredentialAdmin.
//
// Credentials are stored as SecureString parameters. For reading the
// repository supports JSON (username/password, an opaque password-only
// "secret" shape for credentials with no username such as an Azure Service Bus
// SAS connection string, TLS, or capabilities combined with '+') and the simple
// username:password format. Writing serializes as JSON and round-trips the
// payload back through the reader before storing it, so an admin Create/Update
// can never persist a value that a later Get or rotation poll would fail to
// parse.
//
// URI format: pms://namespace/path/to/parameter
// Maps to SSM parameter name: /namespace/path/to/parameter
//
// The scheme is "pms" (Parameter Store) for backward compatibility with
// existing credential URIs.
package ssm
