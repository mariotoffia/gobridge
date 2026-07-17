//go:build !race

package singleton_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/singleton"
)

// Test_TierB_Validation_MultipleGoBridgeInStack covers matrix
// row 10 ("Multiple GoBridge in same stack"). Two GoBridgeSingle
// facades created in the same Stack must panic with the documented
// prefix and a Fix line listing the kind of each instance.
func Test_TierB_Validation_MultipleGoBridgeInStack(t *testing.T) {
	defer jsii.Close()
	t.Cleanup(singleton.ResetForTest)

	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	newSingle(t, stack, "BridgeA")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on second facade in same Stack")
		}
		msg := fmt.Sprintf("%v", r)
		const wantPrefix = "Only one GoBridgeSingle, GoBridgeCluster, or GoBridgeDynamoDBHA instance is supported per stack/account; found 2."
		if !strings.HasPrefix(msg, wantPrefix) {
			t.Fatalf("matrix row 10 prefix mismatch:\n got: %q\nwant: %q", msg, wantPrefix)
		}
		if !strings.Contains(msg, "Fix: remove the extra instance(s)") {
			t.Fatalf("matrix row 10 Fix-guidance missing; got: %s", msg)
		}
		if !strings.Contains(msg, "BridgeA") || !strings.Contains(msg, "BridgeB") {
			t.Fatalf("matrix row 10 must list both construct paths; got: %s", msg)
		}
	}()
	newSingle(t, stack, "BridgeB")
}
