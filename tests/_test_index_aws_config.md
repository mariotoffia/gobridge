# AWS config source construct tests

Local CDK template tests use the existing `!race` convention. They synthesize resources without AWS calls.

| name | description | type | group | status |
|------|-------------|------|-------|--------|
| TestSingle_ConfigTable_RetainedOnDemand | verifies durable config table schema | unit | deployment/cdk | pass |
| TestSingle_ConfigTable_BootstrapCopy | verifies table stamping and caller ownership | unit | deployment/cdk | pass |
| TestSingle_EFS_Conditional | verifies file and SQLite filesystem requirements | unit | deployment/cdk | pass |
| TestSingle_EFSFree_Consumers | verifies alarms and optional SSM exports without EFS | unit | deployment/cdk | pass |
| TestDynamoDBHA_ConfigTable_SharedGrants | verifies one config table and per-role IAM across all task definitions | unit | deployment/cdk | pass |
| TestDynamoDBHA_EFSFree_Consumers | verifies HA mounts, alarms and exports without EFS | unit | deployment/cdk | pass |
| TestCluster_DynamoDBConfig_Rejected | verifies facade rejection of a non-file config source | unit | deployment/cdk | pass |
| TestPhase1_DynamoDBConfig_FilesystemProfile | verifies typed topology rejection | unit | deployment/cdk | pass |
| TestLookupBridge_OptionalEFS | verifies EFS-free cross-stack lookups omit the missing parameter | unit | deployment/cdk | pass |
| TestLookupBridge_WithIncludeARNsLooksUpSixParams | verifies all imports for a known filesystem producer | unit | deployment/cdk | pass |
| TestLookupBridge_AccessorTokensAreNonNil | verifies accessor tokens for a known filesystem producer | unit | deployment/cdk | pass |
