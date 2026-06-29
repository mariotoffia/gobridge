// Package fakesvc is a stand-in vendored SDK for aclcheck analysistest.
// Its import path lives under the github.com/aws/aws-sdk-go-v2/ prefix so
// the analyzer treats it as a vendor SDK.
package fakesvc

// Message is an SDK request/response type.
type Message struct {
	Body string
}

// Client is an SDK client type.
type Client struct {
	Endpoint string
}
