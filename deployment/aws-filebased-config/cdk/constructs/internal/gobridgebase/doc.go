// Package gobridgebase is the shared facade base used by the
// GoBridgeSingle and GoBridgeCluster public constructs.
//
// It owns the parts of the deployment that are identical for the
// single-node and clustered topologies:
//
//   - the Fargate task definition (one per service kind: control vs
//     worker), parameterised by [Mode];
//   - a single EFS volume bound to the appropriate access point
//     exposed by [GoBridgeEfsConfig] (control mounted RW, worker
//     mounted with readOnly:true at the ECS volume layer);
//   - the seeder init container (see internal/seeder), wired with
//     EXPECTED_HASH and MODE env vars so worker tasks gate startup
//     on config drift while control tasks materialise the file;
//   - one CloudWatch log group per container named
//     "/gobridge/<stack-name>/<construct-id>/<container-name>" with
//     RemovalPolicy.RETAIN by default. The stack name is part of it
//     because log-group names are unique per account and region,
//     and a facade always builds its base under a fixed construct
//     id — two deployments of the same shape would otherwise want
//     one name and the second stack could not create;
//   - port mappings derived from the parsed [BridgeConfig] and
//     [BootstrapConfig] — never hard-coded;
//   - task and execution IAM roles plus per-adapter grants applied
//     by iterating over the registry kinds present in the parsed
//     bridge config (see internal/grants).
//
// The package is internal: only the GoBridge{Single,Cluster}
// constructs may import it. Consumers of the public CDK module
// interact with the base indirectly through those facades.
package gobridgebase
