package ports_test

import (
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/mariotoffia/gobridge/ports"
)

// spec/httpapi/config-components.yaml is the machine-readable BridgeConfig
// contract of the Admin API: a client generated from it, or one that validates a
// PATCH overlay against it, only knows the keys the schema lists. A route-policy
// key the parser reads but the schema omits is a knob such a client cannot set;
// a key the schema lists but the parser no longer reads is one it sets and never
// sees take effect.
//
// Both directions are derived from the structs the parser fills, so the schema
// cannot drift from the accepted config without a red test.
//
// Category: unit (TESTS.md §1) — the YAML file is the fixture.

const adminConfigSchema = "../spec/httpapi/config-components.yaml"

// schemaProperties returns the property names the OpenAPI schema declares for
// one component under `schemas:`.
func schemaProperties(t *testing.T, schema string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(adminConfigSchema)
	require.NoError(t, err, "%s must exist", adminConfigSchema)

	var doc struct {
		Schemas map[string]struct {
			Properties map[string]any `yaml:"properties"`
		} `yaml:"schemas"`
	}
	require.NoError(t, yaml.Unmarshal(body, &doc), "%s must be valid YAML", adminConfigSchema)

	def, ok := doc.Schemas[schema]
	require.Truef(t, ok, "%s declares no %s schema", adminConfigSchema, schema)
	props := map[string]bool{}
	for name := range def.Properties {
		props[name] = true
	}
	require.NotEmptyf(t, props, "the %s schema in %s declares no properties", schema, adminConfigSchema)
	return props
}

func TestAdminConfigSchema_RoutePolicyMatchesParsedFields(t *testing.T) {
	for _, c := range []struct {
		schema string
		model  any
	}{
		{"PolicyDef", ports.PolicyDef{}},
		{"BackoffDef", ports.BackoffDef{}},
	} {
		t.Run(c.schema, func(t *testing.T) {
			declared := schemaProperties(t, c.schema)
			parsed := parsedFields(t, c.model)

			var undeclared, phantom []string
			for name := range parsed {
				if !declared[name] {
					undeclared = append(undeclared, name)
				}
			}
			for name := range declared {
				if !parsed[name] {
					phantom = append(phantom, name)
				}
			}
			sort.Strings(undeclared)
			sort.Strings(phantom)
			require.Emptyf(t, undeclared,
				"%T keys the parser reads but the %s schema in %s does not declare; an Admin API client cannot set them",
				c.model, c.schema, adminConfigSchema)
			require.Emptyf(t, phantom,
				"properties the %s schema in %s declares but %T does not read; setting them would be silently ignored",
				c.schema, adminConfigSchema, c.model)
		})
	}
}
