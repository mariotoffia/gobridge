package bootstrap

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/mariotoffia/gobridge/ports"
)

type configLoaderFunc func(context.Context) (*ports.BridgeConfig, error)

func (f configLoaderFunc) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	return f(ctx)
}

type configHTTPClientFunc func(*http.Request) (*http.Response, error)

func (f configHTTPClientFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func configDynamoDBClient(t *testing.T, do configHTTPClientFunc) *dynamodb.Client {
	t.Helper()
	return dynamodb.New(dynamodb.Options{
		Region:       "us-west-2",
		Credentials:  aws.AnonymousCredentials{},
		Retryer:      aws.NopRetryer{},
		HTTPClient:   do,
		BaseEndpoint: aws.String("http://config.invalid"),
	})
}

func configJSONResponse(body string, status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.0"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

var (
	_ ports.Loader        = configLoaderFunc(nil)
	_ dynamodb.HTTPClient = configHTTPClientFunc(nil)
)
