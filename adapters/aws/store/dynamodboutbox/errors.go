package dynamodboutbox

import (
	"errors"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func isConditionFailed(err error) bool {
	var ccf *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

func isTransactionCanceled(err error) bool {
	var tce *ddbtypes.TransactionCanceledException
	return errors.As(err, &tce)
}

func isResourceInUse(err error) bool {
	var riue *ddbtypes.ResourceInUseException
	return errors.As(err, &riue)
}
