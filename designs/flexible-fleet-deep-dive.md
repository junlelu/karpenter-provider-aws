# EC2 Flexible Fleet in Karpenter: Deep Dive Implementation Guide

## Table of Contents

1. [Background: How Karpenter Normally Works](#background-how-karpenter-normally-works)
2. [What is EC2 Flexible Fleet?](#what-is-ec2-flexible-fleet)
3. [The Problem: Why Karpenter Needs Changes](#the-problem-why-karpenter-needs-changes)
4. [Implementation Walkthrough](#implementation-walkthrough)
5. [File-by-File Changes](#file-by-file-changes)
6. [Data Flow: Normal vs Flexible Fleet](#data-flow-normal-vs-flexible-fleet)
7. [Edge Cases and Design Decisions](#edge-cases-and-design-decisions)

---

## Background: How Karpenter Normally Works

### The Provisioning Pipeline

When a pod can't be scheduled (because no node has enough resources), Karpenter's provisioning loop kicks in:

```
Unschedulable Pod Detected
    │
    ▼
CloudProvider.GetInstanceTypes()     ← "What EC2 instance types exist?"
    │                                   Returns: [m5.large, m5.xlarge, r5.large, c5.xlarge, ...]
    │                                   Each has: CPU, Memory, Pods capacity + Offerings (zones/prices)
    ▼
Karpenter Scheduler (bin-packing)    ← "Which instance type fits this pod best?"
    │                                   Picks: m5.xlarge (4 CPU, 16Gi Memory)
    │                                   Creates a NodeClaim with specific requirements
    ▼
CloudProvider.Create(nodeClaim)      ← "Launch an EC2 instance for this NodeClaim"
    │
    ▼
Instance Provider                    ← The actual EC2 API calls happen here
    │
    ├─ filterInstanceTypes()         ← Remove unavailable/incompatible types
    ├─ getLaunchTemplateConfigs()     ← Create EC2 launch templates
    ├─ getOverrides()                ← Create per-zone/type overrides for CreateFleet
    └─ CreateFleet API call          ← Ask EC2 to launch an instance
```

### Key Concepts

**Instance Types**: EC2 machine sizes like `m5.large` (2 CPU, 8Gi), `m5.xlarge` (4 CPU, 16Gi), etc. Each has a `Capacity` (what it can run) and `Offerings` (where/how it's available).

**Offerings**: A specific availability of an instance type. Example: "m5.large is available in us-east-1a as on-demand at $0.096/hr". An offering can be "available" (capacity exists) or "unavailable" (ICE'd — Insufficient Capacity Error).

**NodePool**: A Kubernetes CR that defines a set of constraints for nodes — which instance types are allowed, which zones, spot vs on-demand, etc.

**NodeClaim**: A Kubernetes CR that represents a request for a specific node. Created by the scheduler, fulfilled by the cloud provider.

**EC2NodeClass**: AWS-specific configuration — AMI, security groups, subnets, user data, etc.

**Launch Template**: An EC2 concept that bundles configuration (AMI, user data, security groups) for launching instances.

**CreateFleet API**: The EC2 API Karpenter uses to launch instances. It takes a list of launch template configs with overrides (instance type + zone combinations) and an allocation strategy.

### Normal Allocation Strategies

- **lowest-price** (default for on-demand): EC2 picks the cheapest instance type that has capacity
- **prioritized** (used with node overlays): EC2 uses the priority field to pick instance types in order
- **price-capacity-optimized** (default for spot): EC2 balances price and capacity availability

---

## What is EC2 Flexible Fleet?

EC2 Flexible Fleet is an **internal Amazon** allocation strategy where you tell EC2:

> "Here's a set of instance types. Pick whichever one you want based on your real-time capacity knowledge."

Unlike `lowest-price` which picks the cheapest, or `prioritized` which follows a strict order, `flexible` gives EC2 maximum freedom to choose. This is useful when:

1. You have a "bundle" of instance types that are all acceptable for your workload
2. You want to maximize the chance of getting capacity (EC2 knows more about capacity than you do)
3. You don't care which specific instance type you get

**The key constraint**: Since EC2 can return ANY instance type from your set, you can't assume you'll get a specific one. If you request `[m5.large, m5.xlarge, m5.2xlarge]`, you might get a `m5.large` (2 CPU) or a `m5.2xlarge` (8 CPU).

---

## The Problem: Why Karpenter Needs Changes

Karpenter's normal behavior is designed around the assumption that it controls which instance type gets launched. Several of its mechanisms actively work against flexible fleet:

### Problem 1: Aggressive Filtering

Karpenter filters instance types through 6 filters before sending them to CreateFleet:
- Compatible/available filter (removes types without available offerings)
- Capacity reservation filter
- Capacity block filter
- Reserved offering filter
- Exotic instance type filter
- Spot offering filter

Then it truncates to max 60 types.

**Why this breaks flexible fleet**: If Karpenter thinks `m5.xlarge` is temporarily unavailable (cached ICE error), it removes it from the set. But EC2's flexible strategy might actually have capacity for it. By filtering, Karpenter is second-guessing EC2 and reducing the flexibility.

### Problem 2: Unavailable Offerings Cache

After a CreateFleet call returns errors, Karpenter caches which instance-type/zone combos are unavailable. Next time, it won't even try them.

**Why this breaks flexible fleet**: Over time, Karpenter would progressively mark more and more types as unavailable, shrinking the set until it fails entirely. With flexible fleet, we should always send the full set and let EC2 decide.

### Problem 3: Launch Template Grouping

Karpenter creates separate launch templates for instance types with different `max-pods` values (kubelet configuration depends on instance type). For example:
- m5.large → max-pods=29
- m5.xlarge → max-pods=58
- m5.2xlarge → max-pods=58

These get different launch templates because the kubelet is configured differently.

**Why this breaks flexible fleet**: EC2 flexible fleet expects a single, unified configuration. It can't handle multiple launch templates with different kubelet configs — it needs ONE template that works for whichever type it selects.

### Problem 4: Capacity Assumptions

Karpenter's scheduler looks at each instance type's actual capacity when bin-packing pods. If it sees `m5.2xlarge` has 8 CPU, it might schedule 4 pods that each need 2 CPU onto it.

**Why this breaks flexible fleet**: EC2 might return a `m5.large` (2 CPU) instead of the `m5.2xlarge`. Now those 4 pods won't fit, causing scheduling failures.

### Problem 5: Available-Only Offerings

When building CreateFleet overrides, Karpenter only includes "available" offerings. If an offering was previously ICE'd, it's excluded.

**Why this breaks flexible fleet**: Same as Problem 1 — we're reducing EC2's selection pool based on stale information.

---

## Implementation Walkthrough

### How Users Enable Flexible Fleet

Users add a single annotation to their NodePool template:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: my-flexible-pool
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
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
```

This annotation flows from NodePool → NodeClaim → instance provider, controlling behavior at each stage.

### The Modified Pipeline

```
Unschedulable Pod Detected
    │
    ▼
CloudProvider.GetInstanceTypes()     ← Same as before, BUT:
    │                                   If annotation = "flexible":
    │                                   → adjustCapacityForFlexibleFleet()
    │                                   → All instance types now report min(CPU), min(Memory), min(Pods)
    │                                   Example: m5.large=2CPU, m5.xlarge=4CPU, m5.2xlarge=8CPU
    │                                            → ALL report 2CPU (the minimum)
    ▼
Karpenter Scheduler (bin-packing)    ← Schedules conservatively based on min capacity
    │                                   Won't over-schedule because it assumes worst case
    ▼
CloudProvider.Create(nodeClaim)      
    │
    ▼
resolveInstanceTypes()               ← If flexible: reload requirements from NodePool
    │                                   (bypass Karpenter's requirement pruning)
    │                                   Filter only by NodePool requirements
    ▼
Instance Provider Create()           ← If flexible: SKIP filterInstanceTypes()
    │
    ▼
Resolver.Resolve()                   ← If flexible: group ALL types into single launch template
    │                                   Use max(pods), max(EFA) across all types
    ▼
getLaunchTemplateConfigs()           
    ▼
getOverrides()                       ← If flexible: include ALL offerings (not just available)
    ▼
CreateFleetInputBuilder.Build()      ← Sets AllocationStrategy = "flexible"
    ▼
EC2 CreateFleet API                  ← EC2 picks the best instance type
    ▼
updateUnavailableOfferingsCache()    ← If flexible: SKIP (log errors instead)
```

---

## File-by-File Changes

### 1. `pkg/apis/v1/labels.go` — The Annotation Definition

```go
// AnnotationOnDemandAllocationStrategy allows overriding the EC2 CreateFleet 
// on-demand allocation strategy.
// Supported values: "lowest-price" (default), "prioritized", "flexible".
AnnotationOnDemandAllocationStrategy = apis.Group + "/on-demand-allocation-strategy"
```

**What it does**: Defines the annotation key `karpenter.k8s.aws/on-demand-allocation-strategy`. This is the single entry point for the entire feature.

**Why an annotation**: Annotations on the NodePool template flow through to NodeClaims automatically via Karpenter's core machinery. This means the instance provider can read the annotation from the NodeClaim to determine behavior without any plumbing changes.

---

### 2. `pkg/providers/instance/types.go` — CreateFleet Request Builder

**Changes**:
- Added `allocationStrategy *string` field to `CreateFleetInputBuilder`
- Added `WithAllocationStrategy(strategy string)` method
- Modified `Build()` to use flexible strategy when set

```go
// Before (simplified):
input.OnDemandOptions = &ec2types.OnDemandOptionsRequest{
    AllocationStrategy: "lowest-price",  // always lowest-price or prioritized
}

// After:
if b.allocationStrategy != nil && *b.allocationStrategy == "flexible" {
    allocationStrategy = "flexible"
} else {
    allocationStrategy = lo.Ternary(b.overlay, "prioritized", "lowest-price")
}
input.OnDemandOptions = &ec2types.OnDemandOptionsRequest{
    AllocationStrategy: allocationStrategy,
}
```

**Why**: This is where the actual EC2 API request is built. The `flexible` value is passed directly to EC2's `OnDemandOptions.AllocationStrategy` field.

---

### 3. `pkg/providers/instance/instance.go` — Core Instance Provider

This file has the most changes. Let me walk through each:

#### 3a. Skip Instance Type Filtering

```go
func (p *DefaultProvider) Create(...) {
    // For flexible fleet, skip ALL filtering and use original instance types
    if !IsAllocationStrategyFlexible(nodeClaim) {
        instanceTypes, err = p.filterInstanceTypes(ctx, instanceTypes, nodeClaim)
        // ...
    }
    // ... rest of create
}
```

**What `filterInstanceTypes` normally does**: Runs 6 filters that remove instance types based on availability, compatibility, spot requirements, etc. Then truncates to 60 types.

**Why we skip it**: Flexible fleet needs ALL instance types to be sent to EC2. The filters would remove types that EC2 might actually have capacity for.

#### 3b. Set Flexible Allocation Strategy

```go
// In launchInstance():
allocationStrategy := getOnDemandAllocationStrategy(nodeClaim)
if allocationStrategy == "flexible" {
    cfiBuilder.WithAllocationStrategy(allocationStrategy)
}
```

**What it does**: Reads the annotation from the NodeClaim and passes it to the CreateFleet request builder.

#### 3c. Include All Offerings in Overrides

```go
// In getOverrides():
if IsAllocationStrategyFlexible(nodeClaim) {
    ofs = it.Offerings.Compatible(reqs)           // ALL offerings
} else {
    ofs = it.Offerings.Available().Compatible(reqs)  // only available
}
```

**What "offerings" are**: Each instance type has offerings — one per zone/capacity-type combo. An offering records if that instance-type-in-that-zone is available or has been ICE'd.

**`Available()` vs `Compatible()`**: 
- `Available()` filters to only offerings not marked as unavailable in the cache
- `Compatible(reqs)` filters by scheduling requirements (zone, capacity type) but doesn't filter by availability

**Why we include all**: EC2's flexible strategy has better real-time capacity information than Karpenter's cache. Stale unavailability data shouldn't limit EC2's choices.

#### 3d. Bypass Unavailable Offerings Cache

```go
if IsAllocationStrategyFlexible(nodeClaim) {
    if len(createFleetOutput.Errors) > 0 {
        log.FromContext(ctx).V(1).Info("flexible fleet: skipping unavailable offerings cache update",
            "error-count", len(createFleetOutput.Errors),
            "fleet-id", aws.ToString(createFleetOutput.FleetId))
    }
} else {
    p.updateUnavailableOfferingsCache(...)
}
```

**What the cache normally does**: After CreateFleet returns errors, Karpenter marks those instance-type/zone combos as unavailable for a TTL period. Future launches won't try them.

**Why we skip it**: For flexible fleet, these errors are expected (not all types have capacity at all times). If we cached them, the available set would shrink over time until nothing is left.

**Why we log**: Silent suppression makes debugging impossible. V(1) logging ensures operators can see what's happening when they enable debug logging.

#### 3e. Helper Functions

```go
func getOnDemandAllocationStrategy(nodeClaim *karpv1.NodeClaim) string {
    allocationStrategy, ok := nodeClaim.Annotations[v1.AnnotationOnDemandAllocationStrategy]
    if ok {
        return allocationStrategy
    }
    return "lowest-price"  // default
}

func IsAllocationStrategyFlexible(nodeClaim *karpv1.NodeClaim) bool {
    return getOnDemandAllocationStrategy(nodeClaim) == "flexible"
}
```

`IsAllocationStrategyFlexible` is exported because it's used by `cloudprovider.go` to check the strategy from outside the `instance` package.

---

### 4. `pkg/providers/amifamily/resolver.go` — Launch Template Resolution

#### The Problem

Normally, Karpenter groups instance types by `(maxPods, efaCount, reservationID)` and creates a separate launch template for each group. This is because:
- Different instance types support different numbers of pods (ENI-based networking)
- The kubelet `--max-pods` flag must be set at node boot time via the launch template's user data
- If m5.large supports 29 pods and m5.xlarge supports 58, they need different user data

#### The Solution for Flexible Fleet

```go
if isAllocationStrategyFlexible(nodeClaim) {
    // Use the HIGHEST max-pods value among all instance types
    maxPods := lo.MaxBy(instanceTypes, func(a, b) bool {
        return a.Capacity.Pods().Value() > b.Capacity.Pods().Value()
    })
    maxPodsValue := int(maxPods.Capacity.Pods().Value())
    
    // Use the HIGHEST EFA count
    efaCount := 0
    if efaRequested {
        maxEFA := lo.MaxBy(instanceTypes, ...)
        efaCount = int(maxEFA.Capacity[v1.ResourceEFA].Value())
    }
    
    // ONE launch template for ALL instance types
    resolvedTemplates = append(resolvedTemplates, 
        r.resolveLaunchTemplates(nodeClass, nodeClaim, instanceTypes, ...maxPodsValue, efaCount...))
}
```

**Why max(pods) not min(pods)**: The kubelet's `--max-pods` is an upper limit. Setting it to the maximum means:
- Large instances can run up to their full pod count
- Small instances will still work — they just won't be able to run that many pods (limited by actual resources)
- If we used min(pods), large instances would waste capacity

**The trade-off**: Some instance types will have `--max-pods` set higher than their actual capacity. This is safe because Kubernetes resource accounting (CPU/memory limits) will prevent over-scheduling, and we've already adjusted the advertised capacity to the minimum in `GetInstanceTypes`.

---

### 5. `pkg/cloudprovider/cloudprovider.go` — Cloud Provider Integration

#### 5a. Instance Type Resolution for Create

```go
func (c *CloudProvider) resolveInstanceTypes(ctx, nodeClaim, nodeClass) {
    if instance.IsAllocationStrategyFlexible(nodeClaim) {
        // Get ALL instance types (unfiltered)
        instanceTypes := c.instanceTypeProvider.List(ctx, nodeClass)
        
        // Reload requirements from the NodePool (not the NodeClaim)
        nodePool := c.resolveNodePoolFromNodeClass(ctx, nodeClaim)
        nodeClaim.Spec.Requirements = nodePool.Spec.Template.Spec.Requirements
        reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
        
        // Filter only by NodePool requirements
        return lo.Filter(instanceTypes, func(i) bool {
            return reqs.Compatible(i.Requirements) == nil
        })
    }
    // Normal path: just list
    return c.instanceTypeProvider.List(ctx, nodeClass)
}
```

**Why reload from NodePool**: When Karpenter creates a NodeClaim from a NodePool, it may have already pruned some requirements based on what it thinks is available. For flexible fleet, we want the ORIGINAL requirements from the NodePool to ensure the full set of instance types is included.

**Why filter by requirements at all**: We still need to respect the user's intent. If they said `instance-family: In [m5, r5]`, we shouldn't include `c5` types. The requirements filter ensures only user-requested types are included.

#### 5b. Auto-Compute Min Capacity

```go
func adjustCapacityForFlexibleFleet(instanceTypes) {
    // Find the minimum CPU, Memory, and Pods across all types
    minCPU = min of all instanceTypes[*].Capacity.CPU
    minMemory = min of all instanceTypes[*].Capacity.Memory
    minPods = min of all instanceTypes[*].Capacity.Pods
    
    // Apply to all types (with deep copies to avoid cache pollution)
    for each instanceType:
        capacityCopy = deepCopy(instanceType.Capacity)
        capacityCopy[CPU] = minCPU
        capacityCopy[Memory] = minMemory
        capacityCopy[Pods] = minPods
        instanceType.Capacity = capacityCopy
}
```

**Example**:
| Instance Type | Real CPU | Real Memory | After Adjustment |
|---|---|---|---|
| m5.large | 2 | 8Gi | 2 CPU, 8Gi |
| m5.xlarge | 4 | 16Gi | **2 CPU, 8Gi** |
| m5.2xlarge | 8 | 32Gi | **2 CPU, 8Gi** |
| r5.large | 2 | 16Gi | 2 CPU, **8Gi** |

All types now report the same minimum capacity. Karpenter's scheduler sees them as equivalent and won't over-schedule.

**Why deep copy**: The instance type provider caches instance types. If we modified the cached objects, the changes would persist and affect non-flexible NodePools too. Deep copying ensures we only modify the copies returned for this specific `GetInstanceTypes` call.

---

## Data Flow: Normal vs Flexible Fleet

### Normal Flow

```
NodePool says: use m5.large, m5.xlarge, m5.2xlarge
    │
GetInstanceTypes returns:
    m5.large:   2 CPU, 8Gi  (available in us-east-1a, 1b)
    m5.xlarge:  4 CPU, 16Gi (available in us-east-1a)        ← 1b was ICE'd
    m5.2xlarge: 8 CPU, 32Gi (available in us-east-1a, 1b)
    │
Scheduler picks m5.2xlarge for a 6-CPU pod (only it fits)
    │
Create NodeClaim: requirements=[m5.2xlarge]
    │
filterInstanceTypes: removes types with no available offerings → keeps [m5.2xlarge]
    │
Resolver: creates 1 launch template for m5.2xlarge (maxPods=58)
    │
getOverrides: only available offerings → [m5.2xlarge/us-east-1a, m5.2xlarge/us-east-1b]
    │
CreateFleet(strategy=lowest-price) → gets m5.2xlarge in us-east-1a
```

### Flexible Fleet Flow

```
NodePool says: use m5.large, m5.xlarge, m5.2xlarge + annotation=flexible
    │
GetInstanceTypes returns (after adjustCapacityForFlexibleFleet):
    m5.large:   2 CPU, 8Gi   ← actual minimum
    m5.xlarge:  2 CPU, 8Gi   ← adjusted down from 4/16
    m5.2xlarge: 2 CPU, 8Gi   ← adjusted down from 8/32
    │
Scheduler sees all types as equivalent (2 CPU, 8Gi)
Picks any one (they're all the same capacity now)
    │
Create NodeClaim: requirements=[m5.large, m5.xlarge, m5.2xlarge]
    │
resolveInstanceTypes: reloads from NodePool, returns ALL 3 types
    │
filterInstanceTypes: SKIPPED (flexible fleet)
    │
Resolver: groups ALL into 1 launch template (maxPods=max(29,58,58)=58)
    │
getOverrides: ALL offerings (including ICE'd m5.xlarge/1b)
    → [m5.large/1a, m5.large/1b, m5.xlarge/1a, m5.xlarge/1b, m5.2xlarge/1a, m5.2xlarge/1b]
    │
CreateFleet(strategy=flexible) → EC2 picks whatever has capacity
    │
updateUnavailableOfferingsCache: SKIPPED (logged only)
```

---

## Edge Cases and Design Decisions

### Q: What if the flexible fleet returns a type not in the original set?

This can't happen. EC2 only selects from the instance types provided in the CreateFleet overrides.

### Q: What happens to consolidation/disruption?

Consolidation and disruption still work normally. They use the adjusted capacity (min values) when evaluating whether to consolidate nodes. This means consolidation will be conservative — it won't try to move pods from a large instance to a smaller one if the minimum capacity can't handle it.

### Q: What if all instance types are ICE'd in all zones?

The CreateFleet call returns errors, and Karpenter returns an InsufficientCapacityError to the provisioning loop. The loop will retry with exponential backoff. Since we don't cache the ICE results for flexible fleet, every retry sends the full set to EC2 again.

### Q: Why not use a feature gate?

Feature gates in Karpenter are defined in the core `sigs.k8s.io/karpenter` repo, not in `karpenter-provider-aws`. Adding one requires a cross-repo change. Annotations are a lighter-weight mechanism that can be promoted to a feature gate + spec field later.

### Q: Could the min-capacity adjustment cause waste?

Yes. If you have `[m5.large (2 CPU), m5.24xlarge (96 CPU)]` in the same flexible set, all types will be advertised as 2 CPU. If EC2 returns the m5.24xlarge, 94 CPUs will go unused. **Best practice**: Use instance types of similar size in a flexible fleet bundle.

### Q: What about spot instances with flexible fleet?

Currently, flexible fleet only affects on-demand allocation strategy. Spot uses `price-capacity-optimized` by default with node overlays using `capacity-optimized-prioritized`. Extending to spot is listed as future work.
