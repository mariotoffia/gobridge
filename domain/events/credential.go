package events

import "time"

// Canonical event type for the connectivity/credential aggregate.
const TypeCredentialRotated = "connectivity.credential.rotated"

// Schema version for the credential rotation event.
const SchemaCredentialRotatedV1 SchemaVersion = "1.0.0"

// CredentialRotated is emitted when a credential record is rotated
// (re-issued, re-fetched from the secret store, or replaced by an
// admin call). The event MUST NOT carry secret material -- only the
// metadata required to correlate the rotation with downstream
// session-restart side effects.
type CredentialRotated struct {
	Header
	URI             string `json:"uri,omitempty"`
	Backend         string `json:"backend,omitempty"`
	PreviousVersion string `json:"previous_version,omitempty"`
	NewVersion      string `json:"new_version,omitempty"`
	RotatedBy       string `json:"rotated_by,omitempty"`
}

// NewCredentialRotated constructs the event. credentialID is the
// stable identity of the credential aggregate (e.g. the URI or a
// store-internal identifier). Secret material MUST NOT be passed in.
func NewCredentialRotated(
	eventID, credentialID string,
	occurredAt time.Time,
	uri, backend, previousVersion, newVersion, rotatedBy string,
) CredentialRotated {
	return CredentialRotated{
		Header: newHeader(eventID, TypeCredentialRotated, credentialID,
			occurredAt, SchemaCredentialRotatedV1),
		URI:             uri,
		Backend:         backend,
		PreviousVersion: previousVersion,
		NewVersion:      newVersion,
		RotatedBy:       rotatedBy,
	}
}
