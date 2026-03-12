# EC2 Flexible Fleet Allocation Strategy

## Summary

Add support for EC2's Flexible allocation strategy in Karpenter's AWS provider. When enabled via annotation, Karpenter sets `AllocationStrategy=flexible` on CreateFleet and injects all NodePool-specified instance types into the request, letting EC2 choose the optimal type based on real-time capacity.

> **Note**: EC2 Flexible fleet is currently only available to Amazon internal teams.

## Motivation

EC2's Flexible fleet lets AWS select the optimal instance type from a candidate set based on real-time capacity. This maximizes launch success for workloads that can run on multiple instance types (batch, CI/CD, stateless services).

Today, Karpenter filters instance types aggressively before calling CreateFleet. For flexible fleet, users want ALL specified types sent to EC2 regardless of Karpenter's availability cache.

## Design

### Activation

Single annotation on NodePool template:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  template:
    metadata:
      annotations:
        karpenter.k8s.aws/on-demand-allocation-strategy: "flexible"
    spec:
      requirements:
        - key: karpenter.k8s.aws/instance-family
          operator: In
          values: ["m5", "m6i", "r5", "c5"]
```

### How It Works

The implementation makes **no changes to existing function signatures or pipeline behavior**. It adds logic at two points:

#### 1. CreateFleet Override Injection (`instance.go`)

After Karpenter's normal filtering builds launch template configs, if `flexible` is set:
- Set `OnDemandOptions.AllocationStrategy = "flexible"` on the CreateFleet request
- Read the full instance type list from an in-memory annotation
- Inject any missing types as additional overrides (reusing the existing AMI and subnets)

This ensures EC2 receives the complete set even if Karpenter's filters removed some types.

#### 2. Min-Capacity Scheduling (`cloudprovider.go`)

Since EC2 may return any instance type, the scheduler must not assume a specific type's capacity. In `GetInstanceTypes()`, when flexible is active:
- Compute `min(CPU)`, `min(Memory)`, `min(Pods)` across all types
- Return new InstanceType copies with adjusted capacity
- Original cached types are not mutated

This ensures pods are never over-scheduled onto a smaller instance type EC2 might select.

### Data Flow

```
cloudprovider.Create()
  ├─ List all instance types
  ├─ Save names as in-memory annotation (for injection later)
  └─ Call instanceProvider.Create()
       ├─ filterInstanceTypes() [normal, untouched]
       ├─ launchInstance()
       │    ├─ Build launch templates [normal, untouched]
       │    ├─ If flexible:
       │    │    ├─ Set AllocationStrategy = "flexible"
       │    │    └─ Inject missing types from annotation
       │    └─ CreateFleet API call
       └─ Return instance
```

## Changes

| File | Lines | Description |
|---|---|---|
| `labels.go` | +5 | `AnnotationOnDemandAllocationStrategy` constant |
| `types.go` | +14 | `WithAllocationStrategy` + flexible in `Build()` |
| `instance.go` | +51 | Set strategy + `injectFlexibleFleetOverrides` |
| `cloudprovider.go` | +49 | Annotation passing + `adjustCapacityForFlexibleFleet` |
| `flexible_fleet_test.go` ×2 | +282 | 7 unit tests |

## Trade-offs

| Aspect | Standard | Flexible |
|---|---|---|
| Allocation strategy | lowest-price | flexible (EC2-managed) |
| Instance types in CreateFleet | Filtered subset | Full NodePool set |
| Capacity reporting | Actual per-type | min() across all types |
| Filtering/caching | Normal | Normal (injected types bypass post-filter) |

## Future Work

- **Feature gate**: Gate behind a feature flag (requires core karpenter change)
- **API field**: Promote annotation to `EC2NodeClass` spec field
- **Spot support**: Extend to spot capacity types
- **Consolidation awareness**: Teach consolidation about flexible fleet nodes
