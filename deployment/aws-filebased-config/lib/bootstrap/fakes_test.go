package bootstrap

import (
	"context"
	"encoding/json"
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

// Models a kernel read that cannot return merely because its context expired.
type blockedConfigObserver struct {
	configLoaderFunc
	entered chan struct{}
	release chan struct{}
}

func (o *blockedConfigObserver) Observe(ctx context.Context) (<-chan ports.ConfigObservation, error) {
	close(o.entered)
	<-o.release
	return nil, ctx.Err()
}

type streamingConfigObserver struct {
	configLoaderFunc
	changes chan ports.ConfigObservation
}

func (o *streamingConfigObserver) Observe(context.Context) (<-chan ports.ConfigObservation, error) {
	return o.changes, nil
}

type parameterResolverFunc func(context.Context, string) (string, error)

func (f parameterResolverFunc) ResolveString(ctx context.Context, ref string) (string, error) {
	return f(ctx, ref)
}

type runtimeStartLog struct{ started chan struct{} }

func (l *runtimeStartLog) Write(data []byte) (int, error) {
	var entry struct{ Msg string }
	if json.Unmarshal(data, &entry) == nil && entry.Msg == "runtime starting" {
		select {
		case l.started <- struct{}{}:
		default:
		}
	}
	return len(data), nil
}
