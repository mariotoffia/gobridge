package bootstrap_test

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib/bootstrap"
)

// The bootstrap field reference is the only place an operator can learn which keys
// this deployment profile reads. A field that exists in the parser but not in the
// table is undiscoverable — the coordinated rollout profile is unusable without
// member_id, and nothing else names it — and a field in the table that the parser
// does not read is worse: it is silently ignored, so the operator believes they
// configured something they did not.
//
// These tests derive the parsed set from the struct rather than restating it, so
// the table cannot drift from the model without a red test.

const (
	bootstrapFieldReferenceDoc = "../../../../docs/aws-deployment/configuration.md"
	clusterGuideDoc            = "../../../../docs/cluster/README.md"
)

// docFieldRow matches a Markdown table row whose first cell is a backticked field
// name, e.g. "| `member_id` | `string` | No | ... |".
var docFieldRow = regexp.MustCompile("^\\|\\s*`([a-z0-9_]+)`\\s*\\|")

// documentedBootstrapFields returns the field names listed in the "Field
// Reference" table of the AWS deployment configuration page.
func documentedBootstrapFields(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(bootstrapFieldReferenceDoc)
	require.NoError(t, err, "the bootstrap field reference page must exist")

	fields := map[string]bool{}
	inTable, inFence := false, false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "### Field Reference") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		// A fenced example may contain a line starting with "#" (a YAML comment, a
		// shell prompt). Ending the scan there would silently stop checking the rows
		// below it, so fences are tracked and their contents skipped.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(line, "#") {
			break // the next heading ends the field table
		}
		if match := docFieldRow.FindStringSubmatch(line); match != nil {
			fields[match[1]] = true
		}
	}
	require.NotEmpty(t, fields, "no field rows parsed from %s — the table shape changed",
		bootstrapFieldReferenceDoc)
	return fields
}

// parsedBootstrapFields returns every JSON key BootstrapConfig reads.
func parsedBootstrapFields(t *testing.T) map[string]bool {
	t.Helper()
	typ := reflect.TypeOf(deployinfra.BootstrapConfig{})
	fields := map[string]bool{}
	for i := range typ.NumField() {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = true
	}
	return fields
}

func TestBootstrapFieldReference_DocumentsEveryParsedField(t *testing.T) {
	documented := documentedBootstrapFields(t)
	parsed := parsedBootstrapFields(t)

	var undocumented, phantom []string
	for name := range parsed {
		if !documented[name] {
			undocumented = append(undocumented, name)
		}
	}
	for name := range documented {
		if !parsed[name] {
			phantom = append(phantom, name)
		}
	}
	require.Empty(t, undocumented,
		"bootstrap fields the parser reads but %s does not document; an operator cannot discover them",
		bootstrapFieldReferenceDoc)
	require.Empty(t, phantom,
		"fields documented in %s that the parser does not read; setting them would be silently ignored",
		bootstrapFieldReferenceDoc)
}

// TestBootstrapFieldReference_ParsesTheDocumentedCoordinatedRolloutFields proves
// the documented spellings are the LIVE ones: a table row is only worth something
// if a document written from it reaches the runtime with the values it named.
func TestBootstrapFieldReference_ParsesTheDocumentedCoordinatedRolloutFields(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	doc := map[string]any{
		"bridge_id":                     "gobridge-prod",
		"config_file_path":              "/var/lib/gobridge/bridge.yaml",
		"admin_api_key_param":           "/gobridge/prod/admin-api-key",
		"topology":                      string(deployinfra.TopologyDynamoDBCoordinatedHA),
		"node_role":                     string(deployinfra.NodeRoleWorker),
		"member_id":                     "gobridge-worker-1",
		"metrics_exporter":              deployinfra.MetricsExporterCloudWatch,
		"dynamodb_ha_lease_table_name":  "gobridge-leases",
		"dynamodb_ha_outbox_table_name": "gobridge-outbox",
		"dynamodb_ha_managed_subscriptions_table_name": "gobridge-managed-subscriptions",
		"dynamodb_ha_rollout_table_name":               "gobridge-prod-rollouts",
		"dynamodb_ha_config_fingerprint":               digest,
		"dynamodb_ha_baseline_config_digest":           digest,
	}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)

	cfg, err := bootstrap.LoadBootstrapConfigJSON(raw)
	require.NoError(t, err, "a bootstrap document written from the field reference must load")

	require.Equal(t, "gobridge-worker-1", cfg.MemberID,
		"member_id is the cohort identity the rollout barrier counts acknowledgements against")
	require.Equal(t, "gobridge-prod-rollouts", cfg.DynamoDBHARolloutTableName)
	require.Equal(t, digest, cfg.DynamoDBHAConfigFingerprint)
	require.Equal(t, digest, cfg.DynamoDBHABaselineConfigDigest)
	require.Equal(t, deployinfra.TopologyDynamoDBCoordinatedHA, cfg.Topology)
}

// jsonFence matches a fenced ```json block and captures its body.
var jsonFence = regexp.MustCompile("(?s)```json\\n(.*?)```")

// TestBootstrapFieldReference_PublishedJSONExamplesLoad runs every published
// bootstrap JSON example through the real parser.
//
// A field TABLE proves the keys exist; only the example proves an operator can
// copy the whole document and have it start. Both published examples are the
// first thing a reader pastes, and a stale one fails at task boot with a
// validation error rather than in review.
func TestBootstrapFieldReference_PublishedJSONExamplesLoad(t *testing.T) {
	for _, page := range []string{bootstrapFieldReferenceDoc, clusterGuideDoc} {
		body, err := os.ReadFile(page)
		require.NoError(t, err, "published page must exist")

		examples := 0
		for _, match := range jsonFence.FindAllStringSubmatch(string(body), -1) {
			// Only bootstrap documents: these pages also carry unrelated JSON.
			if !strings.Contains(match[1], `"bridge_id"`) {
				continue
			}
			examples++
			cfg, err := bootstrap.LoadBootstrapConfigJSON([]byte(match[1]))
			require.NoErrorf(t, err, "bootstrap example %d in %s does not load", examples, page)
			require.NotEmptyf(t, cfg.BridgeID, "bootstrap example %d in %s parsed to an empty bridge_id",
				examples, page)
		}
		require.NotZerof(t, examples, "no bootstrap JSON example found in %s — the page or the fence "+
			"shape changed, so this check stopped checking anything", page)
	}
}
