// Package ssm provides an AWS Systems Manager Parameter Store credentials
// repository implementing ports.CredentialRepository and ports.CredentialAdmin.
//
// Credentials are stored as SecureString parameters. The repository supports
// both JSON and simple username:password formats for reading, and serializes
// as JSON for writing.
//
// URI format: pms://namespace/path/to/parameter
// Maps to SSM parameter name: /namespace/path/to/parameter
//
// The scheme is "pms" (Parameter Store) for backward compatibility with
// existing credential URIs.
package ssm
