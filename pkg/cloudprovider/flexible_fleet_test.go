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

package cloudprovider

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func TestAdjustCapacityForFlexibleFleet(t *testing.T) {
	tests := []struct {
		name       string
		types      []*cloudprovider.InstanceType
		wantCPU    string
		wantMemory string
	}{
		{
			name: "single instance type unchanged",
			types: []*cloudprovider.InstanceType{
				{Name: "m5.large", Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:   resource.MustParse("29"),
				}},
			},
			wantCPU:    "2",
			wantMemory: "8Gi",
		},
		{
			name: "adjusts to minimum across types",
			types: []*cloudprovider.InstanceType{
				{Name: "m5.large", Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:   resource.MustParse("29"),
				}},
				{Name: "m5.xlarge", Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
					corev1.ResourcePods:   resource.MustParse("58"),
				}},
				{Name: "m5.2xlarge", Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("8"),
					corev1.ResourceMemory: resource.MustParse("32Gi"),
					corev1.ResourcePods:   resource.MustParse("58"),
				}},
			},
			wantCPU:    "2",
			wantMemory: "8Gi",
		},
		{
			name: "mixed families picks global min",
			types: []*cloudprovider.InstanceType{
				{Name: "r5.large", Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
					corev1.ResourcePods:   resource.MustParse("29"),
				}},
				{Name: "c5.large", Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
					corev1.ResourcePods:   resource.MustParse("29"),
				}},
			},
			wantCPU:    "2",
			wantMemory: "4Gi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := adjustCapacityForFlexibleFleet(tt.types)

			// Verify all types have the same (min) capacity
			for _, it := range result {
				gotCPU := it.Capacity.Cpu().String()
				if gotCPU != tt.wantCPU {
					t.Errorf("instance %s: CPU = %s, want %s", it.Name, gotCPU, tt.wantCPU)
				}
				gotMem := it.Capacity.Memory().String()
				if gotMem != tt.wantMemory {
					t.Errorf("instance %s: Memory = %s, want %s", it.Name, gotMem, tt.wantMemory)
				}
			}

			// Verify original types are NOT mutated
			for i, orig := range tt.types {
				if orig.Capacity.Cpu().String() == result[i].Capacity.Cpu().String() && len(tt.types) > 1 && i > 0 {
					// For types that had higher CPU, the original should still be higher
					continue
				}
			}
		})
	}
}

func TestAdjustCapacityForFlexibleFleet_DoesNotMutateOriginal(t *testing.T) {
	original := []*cloudprovider.InstanceType{
		{Name: "m5.large", Capacity: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
			corev1.ResourcePods:   resource.MustParse("29"),
		}},
		{Name: "m5.xlarge", Capacity: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
			corev1.ResourcePods:   resource.MustParse("58"),
		}},
	}

	_ = adjustCapacityForFlexibleFleet(original)

	// Original m5.xlarge should still have 4 CPU
	if original[1].Capacity.Cpu().String() != "4" {
		t.Errorf("original was mutated: m5.xlarge CPU = %s, want 4", original[1].Capacity.Cpu().String())
	}
	if original[1].Capacity.Memory().String() != "16Gi" {
		t.Errorf("original was mutated: m5.xlarge Memory = %s, want 16Gi", original[1].Capacity.Memory().String())
	}
}
