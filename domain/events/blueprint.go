package events

import "time"

// Canonical event type for the configuration/blueprint aggregate.
const TypeBlueprintCommitted = "config.blueprint.committed"

// Schema version for the blueprint commit event.
const SchemaBlueprintCommittedV1 SchemaVersion = "1.0.0"

// BlueprintCommitted is emitted when a new BridgeConfig blueprint is
// durably committed (replacing any previous active blueprint). The
// event records the commit identity (Revision) and a content hash so
// downstream consumers can deduplicate replays without re-fetching
// the blueprint body.
type BlueprintCommitted struct {
	Header
	Revision    string `json:"revision"`
	ContentHash string `json:"content_hash,omitempty"`
	CommittedBy string `json:"committed_by,omitempty"`
}

// NewBlueprintCommitted constructs the event. blueprintID is the
// stable identifier of the blueprint aggregate (e.g. the file path,
// table key, or logical name) -- not the revision.
func NewBlueprintCommitted(
	eventID, blueprintID string,
	occurredAt time.Time,
	revision, contentHash, committedBy string,
) BlueprintCommitted {
	return BlueprintCommitted{
		Header: newHeader(eventID, TypeBlueprintCommitted, blueprintID,
			occurredAt, SchemaBlueprintCommittedV1),
		Revision:    revision,
		ContentHash: contentHash,
		CommittedBy: committedBy,
	}
}
