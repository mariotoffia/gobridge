package ports_test

import (
	"cmp"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/mariotoffia/gobridge/ports"
)

// spec/httpapi/config-components.yaml is the machine-readable BridgeConfig
// contract of the Admin API: a client generated from it, or one that validates a
// PATCH overlay against it, only knows the keys the schema lists. A key the
// parser reads but the schema omits is a knob such a client cannot set; a key
// the schema lists but the parser no longer reads is one it sets and never sees
// take effect.
//
// The test walks the schema and the ports structs the parser fills in lockstep,
// starting at BridgeConfig. Every property whose Go field holds a ports struct
// must point at a definition through `$ref` or `items.$ref`, and that
// definition is compared with the struct in both directions in turn. A new
// field, struct or schema property anywhere under BridgeConfig therefore cannot
// drift from the accepted config without a red test.
//
// Category: unit (TESTS.md §1) — the YAML file is the fixture.

const adminConfigSchema = "../spec/httpapi/config-components.yaml"

const schemaRefPrefix = "#/schemas/"

// schemaProperty is the part of one OpenAPI property the walk needs: the
// definition it points at. additionalProperties is left undecoded on purpose —
// on `options` it is the bare boolean true, not a schema object.
type schemaProperty struct {
	Ref   string `yaml:"$ref"`
	Items struct {
		Ref string `yaml:"$ref"`
	} `yaml:"items"`
}

type schemaDefinition struct {
	Properties map[string]schemaProperty `yaml:"properties"`
}

// schemaNode pairs a schema definition with the Go struct it must mirror.
type schemaNode struct {
	name string
	typ  reflect.Type
}

func loadAdminConfigSchema(t *testing.T) map[string]schemaDefinition {
	t.Helper()
	body, err := os.ReadFile(adminConfigSchema)
	require.NoError(t, err, "%s must exist", adminConfigSchema)

	var doc struct {
		Schemas map[string]schemaDefinition `yaml:"schemas"`
	}
	require.NoError(t, yaml.Unmarshal(body, &doc), "%s must be valid YAML", adminConfigSchema)
	return doc.Schemas
}

// unwrap strips pointers and slices, leaving the type a field ultimately holds.
func unwrap(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	return typ
}

func TestAdminConfigSchema_MirrorsParsedConfig(t *testing.T) {
	schemas := loadAdminConfigSchema(t)
	root := reflect.TypeFor[ports.BridgeConfig]()

	visited := map[string]reflect.Type{}
	queue := []schemaNode{{name: "BridgeConfig", typ: root}}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if seen, ok := visited[node.name]; ok {
			if seen != node.typ {
				t.Errorf("%s in %s is referenced for both %v and %v; one of those $refs names the wrong definition",
					node.name, adminConfigSchema, seen, node.typ)
			}
			continue
		}
		visited[node.name] = node.typ
		t.Run(node.name, func(t *testing.T) {
			queue = append(queue, mirrorDefinition(t, schemas, node, root.PkgPath())...)
		})
	}
}

// mirrorDefinition compares one definition's property names with the yaml keys
// of its struct and returns the definitions its ports-struct fields point at.
func mirrorDefinition(t *testing.T, schemas map[string]schemaDefinition, node schemaNode, pkg string) []schemaNode {
	t.Helper()
	def, ok := schemas[node.name]
	if !assert.Truef(t, ok, "%s declares no %s schema for %v", adminConfigSchema, node.name, node.typ) {
		return nil
	}

	pluginConfig := reflect.TypeFor[ports.PluginConfig]()
	parsed := parsedFields(t, reflect.Zero(node.typ).Interface())
	var children []schemaNode
	for i := range node.typ.NumField() {
		field := node.typ.Field(i)
		if field.Type == pluginConfig {
			// A PluginConfig field is yaml:"-": the stage-1 parser
			// (config/parser/parse.go) decodes it from the attachment point's
			// `options` map, so `options` is a key the parser reads.
			parsed["options"] = true
			continue
		}
		key, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		prop, declared := def.Properties[key]
		if !parsed[key] || !declared {
			continue // not a yaml key, or undeclared and reported below
		}

		held := unwrap(field.Type)
		if held.Kind() == reflect.Map {
			if elem := unwrap(held.Elem()); elem.Kind() == reflect.Struct && elem.PkgPath() == pkg {
				t.Errorf("%v.%s is a map of %v; the walk cannot follow additionalProperties, so extend it before mirroring this field",
					node.typ, field.Name, elem)
			}
			continue
		}
		if held.Kind() != reflect.Struct || held.PkgPath() != pkg {
			continue // a scalar, or a leaf type from another package
		}
		child, ok := strings.CutPrefix(cmp.Or(prop.Ref, prop.Items.Ref), schemaRefPrefix)
		if !ok || child == "" {
			t.Errorf("%s.%s in %s holds %v but carries no $ref or items.$ref of the form %s<Name>",
				node.name, key, adminConfigSchema, field.Type, schemaRefPrefix)
			continue
		}
		children = append(children, schemaNode{name: child, typ: held})
	}

	var undeclared, phantom []string
	for name := range parsed {
		if _, ok := def.Properties[name]; !ok {
			undeclared = append(undeclared, name)
		}
	}
	for name := range def.Properties {
		if !parsed[name] {
			phantom = append(phantom, name)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(phantom)
	assert.Emptyf(t, undeclared,
		"%v keys the parser reads but the %s schema in %s does not declare; an Admin API client cannot set them",
		node.typ, node.name, adminConfigSchema)
	assert.Emptyf(t, phantom,
		"properties the %s schema in %s declares but %v does not read; setting them would be silently ignored",
		node.name, adminConfigSchema, node.typ)
	return children
}
