//go:build gobridge_aws || gobridge_all

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { expectedFamilies = append(expectedFamilies, "aws") }

// TestAWSFamily_RegistersDecoders verifies transport aliases and the store kind.
func TestAWSFamily_RegistersDecoders(t *testing.T) {
	reg := ports.NewRegistry()
	require.NoError(t, registerAWSDecoders(reg))
	for _, kind := range []string{"sqs", "aws.sqs", "dynamodb"} {
		assert.Contains(t, reg.Kinds(), kind)
	}
	all := ports.NewRegistry()
	require.NoError(t, registerAllDecoders(all))
	for _, kind := range reg.Kinds() {
		assert.Contains(t, all.Kinds(), kind)
	}
}

// TestAWSFamily_ReportsFamily verifies the build reports AWS support.
func TestAWSFamily_ReportsFamily(t *testing.T) {
	assert.Contains(t, compiledFamilies, "aws")
}

// TestAWSFamily_ConfigErrorsReachAggregates verifies neither composition path
// suppresses an SDK configuration error when AWS is linked.
func TestAWSFamily_ConfigErrorsReachAggregates(t *testing.T) {
	isolateAWSConfig(t)

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"wire family", func() error {
			return wireAWSFactories(t.Context(), bridge.NewSupervisor(), discardLogger(), nil)
		}},
		{"wire aggregate", func() error {
			return wireAllFactories(t.Context(), bridge.NewSupervisor(), discardLogger(), nil)
		}},
		{"seed family", func() error {
			return seedAWSStores(t.Context(), bridge.NewBuilder(&ports.BridgeConfig{}))
		}},
		{"seed aggregate", func() error {
			return seedAllStores(t.Context(), bridge.NewBuilder(&ports.BridgeConfig{}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, tc.call())
			t.Setenv("AWS_PROFILE", "missing")
			var loadErr awsconfig.SharedConfigProfileNotExistError
			require.ErrorAs(t, tc.call(), &loadErr)
			assert.Equal(t, "missing", loadErr.Profile)
		})
	}
}

// TestAWSFamily_StoresAndTransportMetrics verifies the aggregate paths open
// managed-subscription stores and both SQS aliases receive the metrics exporter.
// Only the AWS HTTP boundary is faked; the factories and runtime are real.
func TestAWSFamily_StoresAndTransportMetrics(t *testing.T) {
	if testing.Short() {
		t.Skip("local AWS HTTP integration")
	}
	isolateAWSConfig(t)
	serverCtx := t.Context()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		var response any
		switch r.Header.Get("X-Amz-Target") {
		case "DynamoDB_20120810.DescribeTable":
			response = map[string]any{"Table": map[string]any{
				"AttributeDefinitions": []any{map[string]string{
					"AttributeName": "storage_identity", "AttributeType": "S",
				}},
				"KeySchema": []any{map[string]string{
					"AttributeName": "storage_identity", "KeyType": "HASH",
				}},
			}}
		case "AmazonSQS.GetQueueAttributes":
			response = map[string]any{"Attributes": map[string]string{}}
		case "AmazonSQS.ReceiveMessage":
			// No deliveries are needed: Inject exercises the configured sender.
			select {
			case <-r.Context().Done():
			case <-serverCtx.Done():
			}
			return
		case "AmazonSQS.SendMessage":
			response = map[string]string{"MessageId": "sent"}
		default:
			t.Errorf("unexpected AWS operation %q", r.Header.Get("X-Amz-Target"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(response))
	}))
	t.Cleanup(server.Close)
	t.Setenv("AWS_ENDPOINT_URL_DYNAMODB", server.URL)

	for _, kind := range []string{"sqs", "aws.sqs"} {
		t.Run(kind, func(t *testing.T) {
			queueURL := server.URL + "/123456789012/orders"
			plugin := sqsadapter.DefaultConfig()
			plugin.QueueURL, plugin.Endpoint = queueURL, server.URL
			cfg := &ports.BridgeConfig{
				Bridge: ports.BridgeSettings{ID: "aws-family"},
				Stores: ports.StoresConfig{ManagedSubscriptions: &ports.StoreConfig{
					Type: "dynamodb", Config: awsstore.DynamoDBConfig{TableName: "subscriptions"},
				}},
				Receivers: []ports.ReceiverDef{{ID: "in", Transport: kind, Config: &plugin}},
				Senders:   []ports.SenderDef{{ID: "out", Transport: kind, Config: &plugin}},
				Bindings:  []ports.BindingDef{{ID: "target", SenderID: "out", Address: queueURL}},
				Routes: []ports.RouteDef{{
					ID: "route", ReceiverID: "in", Bindings: []string{"target"},
					Policy: ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop", AllowRetryDrop: true},
				}},
			}
			b := bridge.NewBuilder(&ports.BridgeConfig{Bridge: cfg.Bridge, Stores: cfg.Stores},
				bridge.WithLogger(discardLogger()))
			require.NoError(t, seedAllStores(t.Context(), b))
			plan, err := b.Plan(t.Context())
			require.NoError(t, err)
			plan.Close()

			metrics := &ports.RecordingExporter{}
			sup := bridge.NewSupervisor(bridge.WithSupervisorLogger(discardLogger()))
			require.NoError(t, wireAllFactories(t.Context(), sup, discardLogger(), metrics))
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			var runErr error
			t.Cleanup(func() {
				cancel()
				wait.RequireClosed(t, done, 2*time.Second)
				assert.NoError(t, runErr)
			})
			go func() {
				runErr = sup.Run(ctx, cfg, nil)
				close(done)
			}()
			wait.Until(t, 2*time.Second, "Supervisor publishes the AWS runtime", func() bool {
				select {
				case <-done:
					return true
				default:
					return sup.Runtime() != nil
				}
			})
			rt := sup.Runtime()
			require.NotNil(t, rt)
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID: "message", Subject: "orders", Payload: []byte("order"),
			})
			require.NoError(t, rt.Inject(t.Context(), "route", env))
			assert.Len(t, metrics.FindEntries(sqsadapter.MetricSQSSendLatency), 1)
			wait.Until(t, 2*time.Second, "SQS receiver reports through the supplied exporter", func() bool {
				return len(metrics.FindEntries(sqsadapter.MetricSQSMissingRedrivePolicy)) == 1
			})
		})
	}
}

func isolateAWSConfig(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "AWS_") {
			t.Setenv(key, "")
		}
	}
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
}
