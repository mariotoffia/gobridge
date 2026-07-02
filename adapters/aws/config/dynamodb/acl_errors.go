package dynamodb

import (
	"errors"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DynamoDB SDK error classification.
//
// This file is the error-classification half of the dynamodb config
// ACL: it owns the only references to ddbtypes for error sentinel
// checks. Today only the EnsureTable swallow-on-already-exists path
// needs classification; additional mappings should be added here as
// the adapter grows.

// mapEnsureTableError swallows ResourceInUseException (the table
// already exists, which is the success outcome for an idempotent
// EnsureTable call). All other errors are returned unchanged for the
// caller to wrap.
func mapEnsureTableError(err error) error {
	if err == nil {
		return nil
	}
	var inUse *ddbtypes.ResourceInUseException
	if errors.As(err, &inUse) {
		return nil
	}
	return err
}

// isConditionFailed reports whether err is a DynamoDB
// ConditionalCheckFailedException, i.e. a compare-and-set conflict on a
// conditional write (a concurrent Save advanced the version).
func isConditionFailed(err error) bool {
	var ccf *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}
