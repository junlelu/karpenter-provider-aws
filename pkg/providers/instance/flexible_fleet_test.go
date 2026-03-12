/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package instance

import (
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/providers/subnet"
)

func TestInjectFlexibleFleetOverrides_AddssMissingTypes(t *testing.T) {
	// Start with overrides for just m5.large
	configs := []ec2types.FleetLaunchTemplateConfigRequest{
		{
			Overrides: []ec2types.FleetLaunchTemplateOverridesRequest{
				{
					InstanceType:     "m5.large",
					SubnetId:         lo.ToPtr("subnet-1"),
					ImageId:          lo.ToPtr("ami-123"),
					AvailabilityZone: lo.ToPtr("us-east-1a"),
				},
			},
		},
	}

	// NodeClaim with annotation containing full list
	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				v1.AnnotationOnDemandAllocationStrategy + "-instance-types": "m5.large,m5.xlarge,m5.2xlarge",
			},
		},
	}

	zonalSubnets := map[string]*subnet.Subnet{
		"us-east-1a": {ID: "subnet-1", Zone: "us-east-1a"},
		"us-east-1b": {ID: "subnet-2", Zone: "us-east-1b"},
	}

	injectFlexibleFleetOverrides(configs, nodeClaim, zonalSubnets)

	// Should have: m5.large(1 original) + m5.xlarge(2 zones) + m5.2xlarge(2 zones) = 5
	if len(configs[0].Overrides) != 5 {
		t.Errorf("expected 5 overrides, got %d", len(configs[0].Overrides))
	}

	// Check that m5.xlarge and m5.2xlarge are present
	types := make(map[ec2types.InstanceType]bool)
	for _, o := range configs[0].Overrides {
		types[o.InstanceType] = true
	}
	for _, expected := range []ec2types.InstanceType{"m5.large", "m5.xlarge", "m5.2xlarge"} {
		if !types[expected] {
			t.Errorf("missing instance type %s in overrides", expected)
		}
	}
}

func TestInjectFlexibleFleetOverrides_NoAnnotation(t *testing.T) {
	configs := []ec2types.FleetLaunchTemplateConfigRequest{
		{
			Overrides: []ec2types.FleetLaunchTemplateOverridesRequest{
				{InstanceType: "m5.large"},
			},
		},
	}
	nodeClaim := &karpv1.NodeClaim{} // no annotations

	injectFlexibleFleetOverrides(configs, nodeClaim, map[string]*subnet.Subnet{})

	// Should be unchanged
	if len(configs[0].Overrides) != 1 {
		t.Errorf("expected 1 override (unchanged), got %d", len(configs[0].Overrides))
	}
}

func TestInjectFlexibleFleetOverrides_NoDuplicates(t *testing.T) {
	configs := []ec2types.FleetLaunchTemplateConfigRequest{
		{
			Overrides: []ec2types.FleetLaunchTemplateOverridesRequest{
				{InstanceType: "m5.large", SubnetId: lo.ToPtr("subnet-1"), ImageId: lo.ToPtr("ami-123"), AvailabilityZone: lo.ToPtr("us-east-1a")},
				{InstanceType: "m5.xlarge", SubnetId: lo.ToPtr("subnet-1"), ImageId: lo.ToPtr("ami-123"), AvailabilityZone: lo.ToPtr("us-east-1a")},
			},
		},
	}
	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				v1.AnnotationOnDemandAllocationStrategy + "-instance-types": "m5.large,m5.xlarge",
			},
		},
	}

	injectFlexibleFleetOverrides(configs, nodeClaim, map[string]*subnet.Subnet{
		"us-east-1a": {ID: "subnet-1", Zone: "us-east-1a"},
	})

	// Both types already present, nothing should be added
	if len(configs[0].Overrides) != 2 {
		t.Errorf("expected 2 overrides (no duplicates), got %d", len(configs[0].Overrides))
	}
}

func TestWithAllocationStrategy_Flexible(t *testing.T) {
	builder := NewCreateFleetInputBuilder("on-demand", map[string]string{}, nil)
	builder.WithAllocationStrategy("flexible")
	input := builder.Build()

	if input.OnDemandOptions == nil {
		t.Fatal("OnDemandOptions should not be nil")
	}
	if input.OnDemandOptions.AllocationStrategy != ec2types.FleetOnDemandAllocationStrategy("flexible") {
		t.Errorf("expected flexible, got %s", input.OnDemandOptions.AllocationStrategy)
	}
}

func TestWithAllocationStrategy_Default(t *testing.T) {
	builder := NewCreateFleetInputBuilder("on-demand", map[string]string{}, nil)
	input := builder.Build()

	if input.OnDemandOptions == nil {
		t.Fatal("OnDemandOptions should not be nil")
	}
	if input.OnDemandOptions.AllocationStrategy != ec2types.FleetOnDemandAllocationStrategyLowestPrice {
		t.Errorf("expected lowest-price, got %s", input.OnDemandOptions.AllocationStrategy)
	}
}
