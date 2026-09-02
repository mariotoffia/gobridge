# `MirrorTable` — post-deploy DynamoDB schema mirror

Planning artifact for **T12b** (see `testutil/PLAN.md` §4.2, §5.3). Not merged
code. Lands as `deployment/aws-filebased-config/cdk/integration/ddbmirror_local.go`,
build tag `integration_local`.

CloudFormation still creates the table in floci, so the deploy path is
exercised and `AWS::DynamoDB::Table` is proven to provision. This function
copies the resulting schema into DynamoDB Local, where the bridge's data plane
runs with real conditional-write semantics.

```go
// MirrorTable copies a table's schema from src to dst and waits until the
// mirrored table is usable. The bridge's data plane runs against dst, so the
// deploy is still proven by src having produced the schema in the first place.
func MirrorTable(ctx context.Context, src, dst *dynamodb.Client, name string) error {
	desc, err := src.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &name})
	if err != nil {
		return fmt.Errorf("describe %s on source: %w", name, err)
	}
	t := desc.Table

	in := &dynamodb.CreateTableInput{
		TableName:            t.TableName,
		AttributeDefinitions: t.AttributeDefinitions,
		KeySchema:            t.KeySchema,
		StreamSpecification:  t.StreamSpecification,
		// ponytail: always on-demand. DynamoDB Local ignores capacity anyway,
		// and PAY_PER_REQUEST means no throughput to copy on the table or its
		// indexes. Copy t.ProvisionedThroughput only if a test ever asserts it.
		BillingMode: ddbtypes.BillingModePayPerRequest,
	}
	for _, g := range t.GlobalSecondaryIndexes {
		in.GlobalSecondaryIndexes = append(in.GlobalSecondaryIndexes,
			ddbtypes.GlobalSecondaryIndex{
				IndexName:  g.IndexName,
				KeySchema:  g.KeySchema,
				Projection: g.Projection,
			})
	}
	for _, l := range t.LocalSecondaryIndexes {
		in.LocalSecondaryIndexes = append(in.LocalSecondaryIndexes,
			ddbtypes.LocalSecondaryIndex{
				IndexName:  l.IndexName,
				KeySchema:  l.KeySchema,
				Projection: l.Projection,
			})
	}

	var inUse *ddbtypes.ResourceInUseException
	if _, err := dst.CreateTable(ctx, in); err != nil && !errors.As(err, &inUse) {
		return fmt.Errorf("create %s on target: %w", name, err)
	}
	if err := dynamodb.NewTableExistsWaiter(dst).Wait(ctx,
		&dynamodb.DescribeTableInput{TableName: &name}, 30*time.Second); err != nil {
		return fmt.Errorf("wait for %s on target: %w", name, err)
	}

	// TTL is a separate call on both sides; a mirrored lease/outbox table
	// without it would never expire an item and the reaper tests would hang.
	ttl, err := src.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: &name})
	if err != nil {
		return fmt.Errorf("describe TTL for %s on source: %w", name, err)
	}
	d := ttl.TimeToLiveDescription
	if d == nil || d.TimeToLiveStatus != ddbtypes.TimeToLiveStatusEnabled {
		return nil
	}
	_, err = dst.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: &name,
		TimeToLiveSpecification: &ddbtypes.TimeToLiveSpecification{
			AttributeName: d.AttributeName,
			Enabled:       aws.Bool(true),
		},
	})
	if err != nil {
		return fmt.Errorf("enable TTL for %s on target: %w", name, err)
	}
	return nil
}
```

Call it from the local `DeployStack` for every table name in the stack outputs,
before any test traffic starts. Not copied, deliberately: tags, PITR, SSE,
auto-scaling and provisioned capacity — DynamoDB Local has no meaningful
behaviour behind any of them, and each stays a synth assertion.

Two caveats to carry into T12b:

- **`errors.As` is required, not `errors.Is`** — `ResourceInUseException` is a
  struct type. Re-running against a warm DynamoDB Local must be a no-op, not a
  failure, or a second local run cannot start.
- **DynamoDB Local's TTL sweep is lazy**, as it is in real DynamoDB. Tests that
  depend on expiry must drive the reaper or filter expired items on read, never
  wait on the sweeper. `TESTS.md` anti-flake rules apply unchanged.
