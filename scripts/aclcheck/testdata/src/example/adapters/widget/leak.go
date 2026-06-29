package widget

import sdk "github.com/aws/aws-sdk-go-v2/service/fakesvc" // want `vendor SDK import "github.com/aws/aws-sdk-go-v2/service/fakesvc" is allowed only in ACL files`

// rawLeak holds an SDK type in a non-ACL file; the import above is the
// boundary violation the import-confinement rule catches.
var rawLeak sdk.Message
