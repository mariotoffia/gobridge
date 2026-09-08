//go:build !race

package gobridgecdk_test

import (
	"fmt"
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/ssmexports"
	"github.com/stretchr/testify/assert"
)

func efsContextKey(prefix string) string {
	return fmt.Sprintf("ssm:account=%s:parameterName=%s/efs-id:region=%s", testAccount, prefix, testRegion)
}

// TestLookupBridge_OptionalEFS verifies missing and unresolved lookups omit the EFS import.
func TestLookupBridge_OptionalEFS(t *testing.T) {
	for _, tc := range []struct {
		name, efs  string
		unresolved bool
		imports    int
	}{
		{name: "DynamoDB config without filesystem", imports: 4},
		{name: "lookup pending", unresolved: true, imports: 4},
		{name: "missing parameter cached by CDK", efs: "gobridge-no-efs", imports: 4},
		{name: "file config", efs: "fs-12345678", imports: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			context := map[string]interface{}{}
			if !tc.unresolved {
				context[efsContextKey(testPrefix)] = tc.efs
			}
			_, stack := newStack(t, context)
			ref := gobridgecdk.LookupBridge(stack, "Ref", testPrefix, ssmexports.IncludeARNs())
			assert.NotNil(t, ref.AlbARN())
			assert.NotNil(t, ref.ClusterARN())
			assert.Equal(t, tc.imports == 5, ref.EfsID() != nil)
			assert.Equal(t, tc.imports, countSSMImports(t, stack, testPrefix))
		})
	}
}
