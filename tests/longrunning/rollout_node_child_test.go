//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	dynamodblease "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	dynamodbrollout "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbrollout"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// Env keys the parent sets on each rollout child process.
const (
	rolloutNodeEnv        = "GOBRIDGE_ROLLOUT_NODE"
	rolloutNodeMemberEnv  = "GOBRIDGE_ROLLOUT_MEMBER"
	rolloutNodeMembersEnv = "GOBRIDGE_ROLLOUT_MEMBERS" // comma-separated roster
	rolloutNodeBridgeEnv  = "GOBRIDGE_ROLLOUT_BRIDGE"
	rolloutNodeRolloutTbl = "GOBRIDGE_ROLLOUT_ROLLOUT_TABLE"
	rolloutNodeLeaseTbl   = "GOBRIDGE_ROLLOUT_LEASE_TABLE"
	rolloutNodeConfigTbl  = "GOBRIDGE_ROLLOUT_CONFIG_TABLE"
	rolloutNodeTTLEnv     = "GOBRIDGE_ROLLOUT_TTL" // optional; e.g. "4s". Default 2m.
)

// Stdout token prefixes the parent barriers on.
const (
	rolloutTokReady     = "ROLLOUT_READY"     // ROLLOUT_READY:<member>
	rolloutTokCommitted = "ROLLOUT_COMMITTED" // ROLLOUT_COMMITTED:<member>:<version>
	rolloutTokAborted   = "ROLLOUT_ABORTED"   // ROLLOUT_ABORTED:<member>:<generation>
)

// requiredRolloutEnv reads an env var the child cannot run without.
func requiredRolloutEnv(t *testing.T, key string) string {
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("rollout node: required env %s is empty", key)
	}
	return v
}

// TestRolloutNode is the gated child entrypoint: a real bridge.Supervisor running
// the coordinated rollout barrier over the SHARED DynamoDB config source + rollout
// store + lease store. It boots on the config source's current document, watches
// it for candidates, and emits stdout tokens the parent barriers on. It self-skips
// unless launched as a child (env flag set), so a normal `go test` never runs it.
func TestRolloutNode(t *testing.T) {
	if os.Getenv(rolloutNodeEnv) != "1" {
		t.Skip("rollout node helper: launched only as a child process")
	}
	member := requiredRolloutEnv(t, rolloutNodeMemberEnv)
	membersCSV := requiredRolloutEnv(t, rolloutNodeMembersEnv)
	bridgeID := requiredRolloutEnv(t, rolloutNodeBridgeEnv)
	rolloutTable := requiredRolloutEnv(t, rolloutNodeRolloutTbl)
	leaseTable := requiredRolloutEnv(t, rolloutNodeLeaseTbl)
	configTable := requiredRolloutEnv(t, rolloutNodeConfigTbl)
	_ = membersCSV // roster lives in the config document, not needed to build the node

	client := ddblocal.Client(t) // reads DYNAMODB_ENDPOINT from the parent's env

	rolloutStore := dynamodbrollout.NewStore(client, dynamodbrollout.WithTableName(rolloutTable))
	leaseStore := dynamodblease.NewStore(client, dynamodblease.WithTableName(leaseTable))
	loader := ddbconfig.NewLoader(client,
		ddbconfig.WithTableName(configTable),
		ddbconfig.WithBridgeID(bridgeID),
		ddbconfig.WithRegistry(rolloutRegistry()),
		ddbconfig.WithPollInterval(150*time.Millisecond),
	)

	rolloutTTL := 2 * time.Minute
	if v := os.Getenv(rolloutNodeTTLEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			rolloutTTL = d
		}
	}

	encode, decode := rolloutNodeCodec()
	sup := bridge.NewSupervisor(
		bridge.WithSupervisorBlueprintValidator(config.Validate),
		bridge.WithClusterRollout(bridge.ClusterRolloutConfig{
			Store:        rolloutStore,
			Lease:        leaseStore,
			MemberID:     member,
			LeaseTTL:     500 * time.Millisecond,
			PollInterval: 150 * time.Millisecond,
			TTL:          rolloutTTL,
			Encode:       encode,
			Decode:       decode,
		}),
	)
	sup.RegisterTransport("fake", &rolloutFakeTransportFactory{})
	sup.RegisterStoreFactory("memory", &rolloutFakeStoreFactory{})

	mgr := config.NewManager(config.Layer{Name: "ddb", Loader: loader, Watcher: loader})
	t.Cleanup(mgr.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 280*time.Second)
	t.Cleanup(cancel)

	initial, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("rollout node %s: load initial config: %v", member, err)
	}
	changes, err := mgr.Watch(ctx)
	if err != nil {
		t.Fatalf("rollout node %s: watch config: %v", member, err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- sup.Run(ctx, initial, changes) }()

	// Report tokens off observed state. This is a select-with-ctx poll (the
	// sanctioned child pattern), NOT a bare sleep for synchronization.
	announceReady := false
	lastCommitted := -1
	announcedAbortGen := uint64(0)
	for {
		select {
		case err := <-errCh:
			if err != nil {
				fmt.Fprintf(os.Stdout, "ROLLOUT_EXIT:%s:%v\n", member, err)
			}
			return
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Millisecond):
		}
		rt := sup.Runtime()
		cfg := sup.Config()
		if !announceReady && rt != nil && cfg != nil {
			announceReady = true
			fmt.Fprintf(os.Stdout, "%s:%s:%d\n", rolloutTokReady, member, cfg.Version)
		}
		if st, ok := sup.RolloutStatus(); ok {
			if st.State == "committed" && cfg != nil {
				// Emit committed once the member is RUNNING the committed version.
				if applied := memberApplied(sup, cfg); applied && cfg.Version != lastCommitted {
					lastCommitted = cfg.Version
					fmt.Fprintf(os.Stdout, "%s:%s:%d\n", rolloutTokCommitted, member, cfg.Version)
				}
			}
			if st.State == "aborted" && st.Generation != announcedAbortGen {
				announcedAbortGen = st.Generation
				fmt.Fprintf(os.Stdout, "%s:%s:%d\n", rolloutTokAborted, member, st.Generation)
			}
		}
	}
}

// memberApplied reports whether this member is running the committed config
// version (the rollout status Applied flag, cross-checked with the config).
func memberApplied(sup *bridge.Supervisor, cfg *ports.BridgeConfig) bool {
	st, ok := sup.RolloutStatus()
	return ok && st.Applied && st.ConfigVersion == cfg.Version
}
