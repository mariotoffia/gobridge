# AWS config source construct tests

Local CDK template tests use the existing `!race` convention. They synthesize resources without AWS calls.

| name | description | type | group | status |
|------|-------------|------|-------|--------|
| TestSingle_ConfigTable_RetainedOnDemand | verifies durable config table schema | unit | deployment/cdk | pass |
| TestDynamoDBSeeder_AssetAndStartupGate | verifies validated JSON assets, plugin options, exact integer precision and startup grants | unit | deployment/cdk | pass |
| TestDynamoDBSeeder_RejectsWritableWorkerMode | rejects worker seed/overwrite modes instead of granting writes | unit | deployment/cdk | pass |
| TestDynamoDBSeeder_RejectsUnresolvedAssetToken | rejects deploy-time tokens in immutable config assets | unit | deployment/cdk | pass |
| TestDynamoDBSeeder_ModeOverrides | preserves control policies and strict read-only worker override | unit | deployment/cdk | pass |
| TestDynamoDBSeeder_RejectsOversizeAsset | rejects config exceeding the DynamoDB loader data limit at synth | unit | deployment/cdk | pass |
| TestSingle_ConfigTable_BootstrapCopy | verifies table stamping and caller ownership | unit | deployment/cdk | pass |
| TestSingle_EFS_Conditional | verifies file and SQLite filesystem requirements | unit | deployment/cdk | pass |
| TestSingle_EFSFree_Consumers | verifies alarms and optional SSM exports without EFS | unit | deployment/cdk | pass |
| TestDynamoDBHA_ConfigTable_SharedGrants | verifies one config table and per-role IAM across all task definitions | unit | deployment/cdk | pass |
| TestDynamoDBHA_BaselineDigest_SourceVersionIdentity | verifies actual static-slot stamps ignore only DynamoDB source versions and retain exact file identity | unit | deployment/cdk | pass |
| TestDynamoDBHA_BaselineDigest_IncludesEditableContent | verifies baseline stamps cover editable content excluded from deployment admission | unit | deployment/cdk | pass |
| TestDeploymentBaselineContentDigest_IgnoresOnlySourceVersion | verifies version-independent recognition without config mutation or weaker artifact identity | unit | bridge | pass |
| TestBaselineDigests_RejectUncanonicalizableConfig | rejects nil and malformed configs for both digest contracts | unit | bridge | pass |
| TestApp_DynamoDBBaseline_SeedsTheStoredVersion | verifies default/explicit YAML versions recognize fresh and overwritten DynamoDB versions and persist complete generation-zero artifacts | unit | deployment/bootstrap | pass |
| TestApp_DynamoDBBaseline_RejectsChangedContent | rejects an uncommitted edit despite matching deployment profile and a different source version | unit | deployment/bootstrap | pass |
| TestApp_DynamoDBBaseline_RestartBeforeProposalRecoversStoredVersion | verifies a restarted App uses the stored baseline instead of an unproposed DynamoDB edit | unit | deployment/bootstrap | pass |
| TestApp_DynamoDBBaseline_ReportsFullArtifactIdentity | verifies audit/health use the full artifact digest and preserve an established baseline | unit | deployment/bootstrap | pass |
| TestApp_FileBaseline_RejectsVersionOnlyChange | preserves exact-version baseline recognition for file sources | unit | deployment/bootstrap | pass |
| TestDynamoDBHA_EFSFree_Consumers | verifies HA mounts, alarms and exports without EFS | unit | deployment/cdk | pass |
| TestCluster_DynamoDBConfig_Rejected | verifies facade rejection of a non-file config source | unit | deployment/cdk | pass |
| TestPhase1_DynamoDBConfig_FilesystemProfile | verifies typed topology rejection | unit | deployment/cdk | pass |
| TestLookupBridge_OptionalEFS | verifies EFS-free cross-stack lookups omit the missing parameter | unit | deployment/cdk | pass |
| TestLookupBridge_WithIncludeARNsLooksUpSixParams | verifies all imports for a known filesystem producer | unit | deployment/cdk | pass |
| TestLookupBridge_AccessorTokensAreNonNil | verifies accessor tokens for a known filesystem producer | unit | deployment/cdk | pass |
