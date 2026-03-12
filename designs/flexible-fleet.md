# EC2 Flexible Fleet Allocation Strategy

## Summary

This proposal adds support for EC2's Flexible allocation strategy to Karpenter's AWS provider. When enabled, Karpenter delegates instance type selection to EC2's CreateFleet API using the `flexible` allocation strategy, rather than performing its own instance type filtering and prioritization.

> **Note**: The EC2 Flexible fleet allocation strategy is currently only available to Amazon internal teams. This feature is not yet available to external AWS customers. The implementation is designed to be ready for when/if the feature becomes generally available.

## Motivation

EC2's Flexible fleet allocation strategy allows AWS to select the optimal instance type from a set of candidates based on real-time capacity availability. This is particularly useful for workloads that:

- Can run on a wide variety of instance types (e.g., batch processing, CI/CD)
- Want to maximize the probability of getting capacity by letting EC2 choose across many types
- Use pre-defined "bundles" of instance types that are functionally interchangeable

Today, Karpenter aggressively filters instance types based on availability, pricing, and offering status. While this is optimal for the default case, it can be counterproductive for flexible fleet use cases where the user wants ALL instance types in the set to be passed to EC2 for selection.

## Design

### Activation

Flexible fleet is activated via a NodePool template annotation:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: flexible-pool
spec:
  template:
    metadata:
      annotations:
        karpenter.k8s.aws/on-demand-allocation-strategy: "flexible"
    spec:
      requirements:
        - key: karpenter.k8s.aws/instance-family
          operator: In
          values: ["m5", "m6i", "r5", "r6i", "c5", "c6i"]
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
```

### Behavior Changes When Flexible Is Active

The `flexible` allocation strategy modifies Karpenter's behavior in four key areas:

#### 1. Instance Type Filtering Bypass (`instance.go: Create`)

Normal Karpenter aggressively filters instance types through several filters (compatible-available, capacity-reservation, exotic-instance-type, spot-offering, etc.) and truncates to 60 types. For flexible fleet, **all filtering is skipped** because:
- EC2 needs the full set to maximize capacity availability
- Filtering out "unavailable" types defeats the purpose — EC2 may have real-time capacity that Karpenter's cache doesn't reflect

#### 2. Allocation Strategy in CreateFleet (`types.go: Build`)

The CreateFleet API's `OnDemandOptions.AllocationStrategy` is set to `"flexible"` instead of the default `"lowest-price"`. This tells EC2 to use its own optimization algorithm across the full set of instance types.

#### 3. Single Launch Template Grouping (`resolver.go: Resolve`)

Normally, Karpenter creates separate launch templates per max-pods value and EFA count (since different instance types may have different kubelet configurations). For flexible fleet, **all instance types are grouped into a single launch template** using the maximum values:
- `maxPods` = max across all types
- `efaCount` = max across all types (if EFA is requested)

This is necessary because EC2 flexible fleet expects a single configuration to apply to whichever instance type it selects.

#### 4. Unavailable Offerings Cache Bypass (`instance.go: launchInstance`)

Karpenter normally caches ICE (Insufficient Capacity) errors per instance-type/zone pair. For flexible fleet, the cache update is **skipped** because:
- EC2 flexible fleet internally handles capacity-aware selection
- Marking types unavailable would progressively shrink the set, eventually causing failures
- Fleet errors are logged at V(1) for debugging

#### 5. Include All Offerings in Overrides (`instance.go: getOverrides`)

Normally only "available" offerings are included in CreateFleet overrides. For flexible fleet, **all compatible offerings** (available and unavailable) are included, letting EC2 make the final availability determination.

#### 6. Capacity Adjustment for Safe Scheduling (`cloudprovider.go: GetInstanceTypes`)

Since EC2 may return any instance type from the set, Karpenter's scheduler cannot assume a specific type's capacity. The advertised capacity for all instance types is automatically adjusted to `min(CPU)`, `min(Memory)`, and `min(Pods)` across the set. This ensures:
- Pods are never over-scheduled onto a smaller instance type
- The scheduler makes conservative bin-packing decisions
- No manual configuration is needed — the minimum is derived from the actual instance types

### Instance Type Resolution for NodeClaims (`cloudprovider.go: resolveInstanceTypes`)

When a NodeClaim is created with the flexible strategy, the instance type list is resolved by:
1. Listing all instance types from the provider (unfiltered)
2. Looking up the parent NodePool to get the original requirements
3. Filtering only by NodePool requirements (not by availability or resources)

This ensures the full set of NodePool-specified instance types is passed to CreateFleet.

## Trade-offs

| Aspect | Standard | Flexible |
|--------|----------|----------|
| Instance type filtering | Aggressive (6 filters + truncation) | None |
| Allocation strategy | lowest-price / prioritized | flexible (EC2-managed) |
| Launch templates | Per max-pods/EFA/reservation | Single (max values) |
| ICE caching | Per instance-type/zone | Disabled |
| Offerings | Available only | All (available + unavailable) |
| Capacity reporting | Actual per-type | min() across all types |

## Future Work

- **Feature gate**: Gate flexible fleet behind a feature flag (requires core karpenter change)
- **API field**: Promote annotation to a proper `EC2NodeClass` spec field with webhook validation
- **Spot support**: Extend flexible fleet to spot capacity types
- **Metrics**: Add Prometheus metrics for flexible fleet launches (success/failure/selected-type)

## Alternatives Considered

### Manual capacity annotation
The initial implementation used `flex-fleet-bundle-min-cpu` and `flex-fleet-bundle-min-memory` annotations for operators to manually specify the minimum capacity floor. This was replaced with automatic computation because:
- Manual values can be incorrect or stale
- Auto-compute is always correct by definition
- Reduces operator burden

### Per-instance-type launch templates
Considered keeping the normal launch template grouping for flexible fleet but found that EC2's flexible strategy works best with a single, unified configuration.
