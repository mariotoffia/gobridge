package ports_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// The route-policy tables are where an operator discovers what bounds a
// retrying message. `replay_budget` is the half of the poison gate nobody could
// find: poisoning requires BOTH the attempt count and the wall-clock budget to
// be spent, so an operator who raises max_replay_attempts and sees nothing
// change has no page telling them the 15-minute clock is the binding half. A
// key the table invents is the mirror failure — it gets configured and never
// takes effect.
//
// Both directions are derived from the structs the parser fills, so neither
// table can drift from the accepted config without a red test.

const routePolicyDoc = "../docs/routes-and-runtime-reference.md"

func TestRoutePolicyReference_DocumentsEveryParsedField(t *testing.T) {
	for _, c := range []struct {
		heading string
		model   any
	}{
		{"### `routes[].policy` --", ports.PolicyDef{}},
		{"### `routes[].policy.backoff` --", ports.BackoffDef{}},
	} {
		t.Run(c.heading, func(t *testing.T) {
			documented := documentedFields(t, routePolicyDoc, c.heading)
			parsed := parsedFields(t, c.model)

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
			require.Emptyf(t, undocumented,
				"policy keys the parser reads but %s does not document; an operator cannot discover them",
				routePolicyDoc)
			require.Emptyf(t, phantom,
				"keys documented under %q that the parser does not read; setting them would be silently ignored",
				c.heading)
		})
	}
}
