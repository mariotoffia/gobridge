package integration_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/bridge"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// What a committed config must BE, and why a cohort depends on it.
//
// Every other member of a cluster learns of a change by PARSING the document the
// admin commit wrote, and parsing decodes each entry's typed plugin options. An
// admin overlay carries none of those options — the blueprint def types tag their
// Config `json:"-"` — so the merged value this process holds in memory is not the
// value the document produces.
//
// The cohort agrees on a change by its canonical digest, so those two values are
// two different changes: the member that committed stages one identity and every
// peer derives the other, no peer can join the proposal, and the rollout
// deadline-aborts having collected the proposer's own acknowledgement and nothing
// else. Applying what was written rather than what was merged is what closes it,
// and that is what these tests pin.

// commitCapturingConfigAPI is the admin config API over a real file store, with
// the config each successful commit hands to the runtime captured.
type commitCapturingConfigAPI struct {
	URL     string
	Path    string
	Applied func() *ports.BridgeConfig
}

func newCommitCapturingConfigAPI(t *testing.T, baseCfg *ports.BridgeConfig) commitCapturingConfigAPI {
	t.Helper()

	cfgPath := t.TempDir() + "/config.yaml"
	if err := cfgparser.WriteFile(cfgPath, baseCfg); err != nil {
		t.Fatalf("write base config: %v", err)
	}

	store := &cfgparser.FileStore{Path: cfgPath, Registry: newTestRegistry()}
	current, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("load base config: %v", err)
	}

	rt := goruntime.New(goruntime.WithInstanceID("candidate-identity-test"))
	srv := httpapi.New(rt, httpapi.Config{
		AdminAddr:          ":0",
		MonitorAddr:        ":0",
		AdminAPIKey:        shared.NewSecret(testAdminAPIKey),
		MonitorAPIKey:      shared.NewSecret(testMonitorAPIKey),
		RuntimeProvider:    func() ports.Runtime { return rt },
		ConfigStore:        store,
		ConfigProvider:     func() *ports.BridgeConfig { return current },
		ConfigSingleWriter: true,
		ConfigApplier: func(_ context.Context, cfg *ports.BridgeConfig) error {
			current = cfg
			return nil
		},
	}, httpapi.WithServerLogger(nil))
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.WithoutCancel(t.Context())) })

	return commitCapturingConfigAPI{
		URL:     srv.AdminURL(),
		Path:    cfgPath,
		Applied: func() *ports.BridgeConfig { return current },
	}
}

// subscribedReceiverConfig is a config whose receiver carries one subscription
// with typed plugin options — the shape an admin overlay cannot express, and
// therefore the shape that tells the merged value and the document apart.
func subscribedReceiverConfig() *ports.BridgeConfig {
	cfg := baseConfigForAPI()
	topic := ports.SubscriptionDef{Topic: "topic/a"}
	topic.SetDecoded(fakeOverlayConfig{Broker: "broker-a"}, nil)
	cfg.Receivers[0].Topics = []ports.SubscriptionDef{topic}
	return cfg
}

// addTopicOverlay is the smallest change an operator can make to a receiver's
// subscription list through the admin API: keep the existing topic and add one.
func addTopicOverlay() map[string]any {
	return map[string]any{
		"receivers": []map[string]any{{
			"id":     "rx-1",
			"topics": []map[string]any{{"topic": "topic/a"}, {"topic": "topic/b"}},
		}},
	}
}

// TestConfigAPI_CommittedConfigIsTheDocumentEveryMemberLoads pins the cohort
// agreement property: the config a commit hands the runtime must canonicalise to
// the same digest as an independent load of the document it just wrote. When it
// does not, a coordinated cohort has two identities for one change and can never
// reach agreement on it.
func TestConfigAPI_CommittedConfigIsTheDocumentEveryMemberLoads(t *testing.T) {
	api := newCommitCapturingConfigAPI(t, subscribedReceiverConfig())

	txn := createTransaction(t, api.URL, testAdminAPIKey)
	applyOverlay(t, api.URL, testAdminAPIKey, txn, addTopicOverlay())
	commitTransaction(t, api.URL, testAdminAPIKey, txn)

	applied, err := bridge.ConfigArtifactDigest(api.Applied())
	if err != nil {
		t.Fatalf("digest the applied config: %v", err)
	}
	document, err := bridge.ConfigArtifactDigest(readConfigFromDisk(t, api.Path))
	if err != nil {
		t.Fatalf("digest the committed document: %v", err)
	}
	if applied != document {
		t.Fatalf("the committed config and the document it wrote are two different changes "+
			"(applied=%s document=%s); every peer derives the document's identity, so none of "+
			"them could ever join this member's proposal", applied, document)
	}
}

// TestConfigAPI_SubscriptionOptionsSurviveATopicListPatch pins the corruption
// the same overlay causes on a single node, where no cohort is involved: a patch
// that repeats an existing topic in order to add a second one must not erase the
// options the repeated topic already carries. They live only in the typed Config,
// which the overlay's wire form cannot express, so a wholesale replacement writes
// a document with the options gone for good.
func TestConfigAPI_SubscriptionOptionsSurviveATopicListPatch(t *testing.T) {
	api := newCommitCapturingConfigAPI(t, subscribedReceiverConfig())

	txn := createTransaction(t, api.URL, testAdminAPIKey)
	applyOverlay(t, api.URL, testAdminAPIKey, txn, addTopicOverlay())
	commitTransaction(t, api.URL, testAdminAPIKey, txn)

	document := readConfigFromDisk(t, api.Path)
	topics := document.Receivers[0].Topics
	if len(topics) != 2 {
		t.Fatalf("the committed document has %d topics, want the patched list of 2", len(topics))
	}
	kept, ok := topics[0].Config.(fakeOverlayConfig)
	if !ok {
		t.Fatalf("topic %q lost its typed options entirely (%T)", topics[0].Topic, topics[0].Config)
	}
	if kept.Broker != "broker-a" {
		t.Fatalf("topic %q came back with broker %q, want the %q it was committed with: adding a "+
			"sibling topic erased it", topics[0].Topic, kept.Broker, "broker-a")
	}
}
