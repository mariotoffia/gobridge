//go:build !race

package gobridgealbattachment_test

import (
	"fmt"
	"testing"

	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
)

// Test_TierB_Validation_ALBPriorityCollision covers matrix row 13.
// Matrix wording:
//
//	"ALB BasePriority N reserves [N..N+99]; consumer rule already uses N+M"
//
// A consumer rule planted at priority 125 inside the reserved
// [100..199] window of the default-priority attachment must trigger
// a panic with the exact documented sentence.
func Test_TierB_Validation_ALBPriorityCollision(t *testing.T) {
	defer jsii.Close()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)

	dummyTG := elbv2.NewApplicationTargetGroup(stack, jsii.String("DummyTG"), &elbv2.ApplicationTargetGroupProps{
		Vpc: vpc, Port: jsii.Number(8080), Protocol: elbv2.ApplicationProtocol_HTTP, TargetType: elbv2.TargetType_IP,
	})
	elbv2.NewApplicationListenerRule(stack, jsii.String("ConsumerRule"), &elbv2.ApplicationListenerRuleProps{
		Listener: listener,
		Priority: jsii.Number(125),
		Conditions: &[]elbv2.ListenerCondition{
			elbv2.ListenerCondition_PathPatterns(&[]*string{jsii.String("/consumer/*")}),
		},
		TargetGroups: &[]elbv2.IApplicationTargetGroup{dummyTG},
	})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on priority collision")
		}
		const want = "ALB BasePriority 100 reserves [100..199]; consumer rule already uses 100+25"
		if got := fmt.Sprintf("%v", r); got != want {
			t.Fatalf("matrix row 13 wording drift:\n got: %s\nwant: %s", got, want)
		}
	}()
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
}
