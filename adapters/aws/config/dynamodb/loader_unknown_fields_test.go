package dynamodb_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// brokerConfig is a plugin config whose decoder reads its options through
// RawConfig.Decode, the way every adapter decoder does, so an unknown option
// reaches the same decode an adapter runs.
type brokerConfig struct {
	Broker string `json:"broker"`
}

func (c *brokerConfig) Kind() string { return "mqtt" }
func (c *brokerConfig) Validate() error {
	if c.Broker == "" {
		return errors.New("broker is required")
	}
	return nil
}

// TestLoad_IgnoresUnknownFieldsInAStoredDocument writes the row a newer release
// stores: its document carries a top-level section, a bridge setting and a
// plugin option this release does not know. Load reads the fields it knows and
// ignores the rest, so a downgraded task still starts on that document.
func TestLoad_IgnoresUnknownFieldsInAStoredDocument(t *testing.T) {
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable("cfg-unknown")
	reg := ports.NewRegistry()
	if err := reg.Register("mqtt", func(raw ports.RawConfig) (ports.PluginConfig, error) {
		var c brokerConfig
		if err := raw.Decode(&c); err != nil {
			return nil, err
		}
		return &c, c.Validate()
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	loader := ddbconfig.NewLoader(client,
		ddbconfig.WithTableName(tableName),
		ddbconfig.WithBridgeID("test-bridge"),
		ddbconfig.WithRegistry(reg),
	)
	ctx := context.Background()
	if err := loader.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	document := `{"version":3,
		"bridge":{"id":"test-bridge","log_level":"info","future_setting":"x"},
		"future_section":{"enabled":true},
		"sessions":[{"id":"mqtt-1","transport":"mqtt",
			"options":{"broker":"tcp://broker:1883","future_option":5}}]}`
	if _, err := client.PutItem(ctx, &awsddb.PutItemInput{
		TableName: aws.String(tableName),
		Item: map[string]ddbtypes.AttributeValue{
			"PK":      &ddbtypes.AttributeValueMemberS{Value: "config#test-bridge"},
			"SK":      &ddbtypes.AttributeValueMemberS{Value: "current"},
			"data":    &ddbtypes.AttributeValueMemberS{Value: document},
			"version": &ddbtypes.AttributeValueMemberN{Value: "3"},
		},
	}); err != nil {
		t.Fatalf("put stored document: %v", err)
	}

	got, err := loader.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Version != 3 || got.Bridge.ID != "test-bridge" || got.Bridge.LogLevel != "info" {
		t.Fatalf("known fields: version=%d id=%q log_level=%q", got.Version, got.Bridge.ID, got.Bridge.LogLevel)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("sessions: got %d, want 1", len(got.Sessions))
	}
	cfg, ok := got.Sessions[0].Config.(*brokerConfig)
	if !ok {
		t.Fatalf("session config type %T", got.Sessions[0].Config)
	}
	if cfg.Broker != "tcp://broker:1883" {
		t.Errorf("broker option: got %q", cfg.Broker)
	}
}
