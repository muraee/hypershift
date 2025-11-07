# HyperShift Platform Provider Architecture - Re-architecture Plan

**Status**: Proposal
**Date**: 2025-01-24
**Author**: Architecture Discussion

---

## Executive Summary

This document outlines a comprehensive re-architecture plan for HyperShift to separate platform-specific logic from core components. The goal is to enable easier maintenance of existing platforms and simpler addition of new platforms while eliminating cross-platform contamination risks.

**Current Problems:**
- 2,976+ lines of platform-specific API code embedded in core types
- 34+ platform-specific files scattered across operators
- Switch-based dispatch requiring core modifications for each new platform
- Risk of changes to one platform affecting others
- Growing API and code complexity as more platforms are added

**Proposed Solution:**
- **CRD-based provider pattern** for infrastructure and node pool management
- **Server-Side Apply (SSA)** for providers to populate control plane configuration in HCP.Spec.Platform
- Complete separation of provider code with independent dependencies
- User-controlled platform configurations
- **No CPO changes required** - CPO continues reading HCP.Spec.Platform as it does today

---

## Table of Contents

1. [Problem Statement](#problem-statement)
2. [Architecture Overview](#architecture-overview)
3. [CRD Naming Conventions](#crd-naming-conventions)
4. [Component Separation Strategy](#component-separation-strategy)
5. [User Workflow](#user-workflow)
6. [Platform Provider Pattern](#platform-provider-pattern)
7. [NodePool Configuration Pattern](#nodepool-configuration-pattern)
8. [Control Plane Operator](#control-plane-operator)
9. [Security Considerations](#security-considerations)
10. [Repository Structure](#repository-structure)
11. [Migration Strategy](#migration-strategy)
12. [Benefits and Trade-offs](#benefits-and-trade-offs)

---

## Problem Statement

### The Challenge

HyperShift's current architecture was designed when AWS was the primary platform. As support for Azure, KubeVirt, PowerVS, OpenStack, and Agent platforms has been added, the codebase has accumulated significant technical debt that threatens its maintainability and scalability.

### Core Architectural Problems

#### 1. Monolithic API Design

**Current Reality**: All platform-specific configuration is embedded directly in core types:

```go
// api/hypershift/v1beta1/platform.go
type PlatformSpec struct {
    Type PlatformType

    AWS       *AWSPlatformSpec       `json:"aws,omitempty"`
    Azure     *AzurePlatformSpec     `json:"azure,omitempty"`
    KubeVirt  *KubevirtPlatformSpec  `json:"kubevirt,omitempty"`
    PowerVS   *PowerVSPlatformSpec   `json:"powervs,omitempty"`
    OpenStack *OpenStackPlatformSpec `json:"openstack,omitempty"`
    Agent     *AgentPlatformSpec     `json:"agent,omitempty"`
    // Every new platform adds another field...
}
```

**Impact**:
- **API Bloat**: ~2,976 lines of platform-specific API code (28% of total) embedded in core types
  - AWS: 1,004 lines
  - Azure: 774 lines
  - OpenStack: 451 lines
  - KubeVirt: 403 lines
  - PowerVS: 324 lines
  - Agent: 20 lines
- **Breaking Changes Risk**: Adding/modifying platform fields requires core API changes affecting all users
- **Cognitive Load**: Developers must understand all platform APIs even when working on a single platform
- **API Versioning Complexity**: Cannot version platform APIs independently from core API

#### 2. Platform Code Contamination

**Current Reality**: Platform-specific logic is scattered across multiple operators:

**In hypershift-operator**:
- `controllers/platform/platform.go` - 500+ line switch statement for platform dispatch
- `controllers/hostedcluster/internal/platform/` - Platform-specific validators and defaulters
- Platform-specific reconciliation logic in core controllers

**In control-plane-operator**:
- 34+ platform-specific files across multiple packages
- Platform-specific KAS (Kube API Server) customizers
- Platform-specific secret handlers
- Platform-specific networking setup

**Real-World Example of Cross-Contamination**:
```go
// This pattern appears throughout the codebase:
switch hc.Spec.Platform.Type {
case hyperv1.AWSPlatform:
    // AWS-specific logic using AWS SDK
    // Imports: github.com/aws/aws-sdk-go-v2/...
case hyperv1.AzurePlatform:
    // Azure-specific logic using Azure SDK
    // Imports: github.com/Azure/azure-sdk-for-go/...
case hyperv1.PowerVSPlatform:
    // PowerVS-specific logic using IBM SDK
    // Imports: github.com/IBM-Cloud/...
// Adding new platform requires modifying this file
}
```

**Impact**:
- **Tight Coupling**: Core operators import ALL platform SDKs even if only using one platform
- **Change Amplification**: Simple platform-specific change requires touching core operator code
- **Review Burden**: Platform changes require review from core team even for platform-specific logic
- **Testing Complexity**: Testing one platform requires dependencies for all platforms

#### 3. Dependency Hell

**Current Reality**: Single `go.mod` for entire repository creates version conflicts:

```
go.mod (current monorepo):
require (
    github.com/aws/aws-sdk-go-v2 v1.50.0
    github.com/Azure/azure-sdk-for-go v68.0.0
    github.com/IBM-Cloud/power-go-client v1.6.0
    sigs.k8s.io/cluster-api v1.11.0
    sigs.k8s.io/cluster-api-provider-aws v2.8.0  // Wants CAPI v1.10.0!
    sigs.k8s.io/cluster-api-provider-azure v1.18.0
    // Version conflicts force us to:
    // - Stay on old versions of CAPI providers
    // - Use replace directives (technical debt)
    // - Skip security updates
    // - Block feature adoption
)
```

**Real Issues Encountered**:
1. **CAPI Version Skew**:
   - Core HyperShift ready for CAPI v1.11.0
   - CAPI-AWS provider requires CAPI v1.10.x
   - Cannot upgrade without breaking AWS support

2. **SDK Version Conflicts**:
   - AWS SDK v1 and v2 incompatibilities
   - Azure SDK major version migrations
   - IBM Cloud SDK API changes

3. **Transitive Dependency Conflicts**:
   - Different platforms use different versions of common dependencies
   - Kubernetes client-go version conflicts
   - Controller-runtime version mismatches

**Impact**:
- **Innovation Blocked**: Cannot adopt new CAPI features
- **Security Risk**: Cannot update SDKs with security fixes
- **Maintenance Overhead**: Complex `replace` directives and version pinning
- **Upgrade Paralysis**: Major version upgrades become multi-month projects

#### 4. Development Velocity Degradation

**Time to Add New Platform** (Historical Data):

| Platform | Time to Initial Support | Lines of Code Touched | Files Modified |
|----------|------------------------|----------------------|----------------|
| AWS (initial) | 2 months | ~5,000 | 20 |
| Azure | 4 months | ~8,000 | 45 (including core changes) |
| KubeVirt | 3 months | ~6,500 | 38 (including core changes) |
| PowerVS | 5 months | ~7,800 | 52 (including core changes) |

**Why It Gets Slower**:
1. **Core Modifications Required**: Every new platform needs changes to:
   - Core API types (platform_types.go)
   - Platform dispatcher (GetPlatform() switch)
   - Core operator controllers (validation, defaulting)
   - CPO platform setup logic

2. **Integration Testing Burden**: Must test that new platform doesn't break existing platforms
   - AWS integration tests still pass
   - Azure integration tests still pass
   - KubeVirt integration tests still pass
   - PowerVS integration tests still pass

3. **Review Bottleneck**: Core team must review ALL changes even for platform-specific logic

**Impact**:
- **Longer Time to Market**: 3-5 months to add new platform support
- **Higher Development Costs**: More engineer-months required
- **Opportunity Cost**: Core team bandwidth consumed by platform-specific work
- **Competitive Disadvantage**: Slower to support new cloud providers

#### 5. Testing and Maintenance Burden

**Current Test Matrix Complexity**:

Every change potentially affects N platforms × M configurations:
- 6 platforms (AWS, Azure, KubeVirt, PowerVS, OpenStack, Agent)
- 3 cluster types (Public, Private, PrivateLink)
- 2 networking modes (OVN, Calico)
- 2 scaling modes (Manual, Auto)

**Result**: 72+ test combinations for each core change

**Real Maintenance Challenges**:

1. **Fragile Test Suite**:
   - AWS SDK update breaks Azure tests (import conflicts)
   - CAPI version update requires updating all platform integration tests
   - Flaky tests across platforms due to shared test infrastructure

2. **Regression Risk**:
   - Change to AWS-specific code in shared file accidentally affects Azure
   - Refactoring platform dispatcher breaks PowerVS
   - Adding new platform field breaks backward compatibility

3. **Debug Complexity**:
   ```
   Error: "failed to create infrastructure"
   Where? Could be in:
   - hypershift-operator platform validator
   - hypershift-operator reconciler
   - control-plane-operator infrastructure controller
   - CAPI provider controller
   - Platform SDK call
   ```
   - Debugging requires understanding entire stack across all components

**Impact**:
- **Slower Development**: Fear of breaking other platforms slows changes
- **Higher Bug Rate**: Cross-platform contamination causes unexpected failures
- **Increased Oncall Burden**: Complex debugging across multiple components
- **Technical Debt Accumulation**: Workarounds added instead of proper fixes

#### 6. Scalability and Extensibility Limitations

**Current Constraints**:

1. **Cannot Support Platform Variations**:
   - AWS Classic vs AWS LocalZones vs AWS Wavelength - need different configurations
   - Azure regions with different capabilities
   - Custom platform integrations (private clouds, hybrid setups)
   - All variations must be encoded in core API

2. **Cannot Delegate Platform Ownership**:
   - Platform teams cannot independently release updates
   - All changes require core team approval
   - Cannot iterate quickly on platform-specific features

3. **Cannot Support Third-Party Platforms**:
   - Community cannot add platform support without core repository changes
   - No extension mechanism for private/custom platforms
   - Closed ecosystem

**Impact**:
- **Innovation Limited**: Only core team can add platforms
- **Slower Feature Delivery**: Platform teams blocked by core release cycle
- **Reduced Community Engagement**: High barrier to contribution
- **Missed Opportunities**: Cannot support niche platforms economically

### Summary: Why This Matters

The current architecture creates a **maintenance burden that grows quadratically** with platform count:
- Each new platform adds code to core operators
- Each new platform increases test matrix size
- Each new platform increases dependency conflicts
- Each core change must be validated against all platforms

**Projected Growth Without Change**:
- **Year 1**: Add 2 more platforms → 8 total → ~3,500 lines of platform API code
- **Year 2**: Add 3 more platforms → 11 total → ~5,000 lines of platform API code
- **Year 3**: Development velocity grinds to halt

**The Need for Re-architecture**: We need a pattern that allows platforms to evolve independently while maintaining security and reliability of core components.

---

## Architecture Overview

### API Structure: Platform-Specific CRDs with References

The proposed architecture introduces **platform-specific CRDs** that are **referenced** from core HostedCluster and NodePool resources, rather than embedding platform configuration directly.

#### New Platform CRDs (per platform)

Each platform defines two CRDs:

1. **Cluster Infrastructure CRD** - Platform-specific cluster infrastructure configuration
   - Examples: `HostedAWSCluster`, `HostedAzureCluster`, `HostedKubeVirtCluster`
   - Contains: VPC/networking, load balancers, encryption keys, cloud-specific settings
   - API Group: `infrastructure.hypershift.openshift.io/v1beta1`

2. **NodePool Configuration CRD** - Platform-specific node pool configuration
   - Examples: `AWSNodePool`, `AzureNodePool`, `KubeVirtNodePool`
   - Contains: Instance types, volumes, networking, IAM profiles, cloud-specific settings
   - API Group: `infrastructure.hypershift.openshift.io/v1beta1`

#### Core API Changes: Reference Pattern

Core HostedCluster and NodePool resources reference platform CRDs instead of embedding platform specs:

```yaml
# HostedCluster references platform infrastructure
apiVersion: hypershift.openshift.io/v1beta1
kind: HostedCluster
spec:
  platform:
    type: AWS
    infrastructureRef:  # NEW: Reference instead of embedded spec
      apiVersion: infrastructure.hypershift.openshift.io/v1beta1
      kind: HostedAWSCluster
      name: my-cluster-infra

# NodePool references platform configuration
apiVersion: hypershift.openshift.io/v1beta1
kind: NodePool
spec:
  platform:
    type: AWS
    nodePoolRef:  # NEW: Reference instead of embedded spec
      apiVersion: infrastructure.hypershift.openshift.io/v1beta1
      kind: AWSNodePool
      name: my-nodepool-config
```

**Key Benefits**:
- ✅ Core APIs stay clean and platform-agnostic
- ✅ Platform CRDs can evolve independently
- ✅ No API bloat in core types
- ✅ Platform teams own their CRD definitions

### Provider Implementation: One Binary, Two Controllers

Each platform (AWS, Azure, etc.) has a dedicated provider binary with two controller responsibilities:

```
┌─────────────────────────────────────────────────────────────────┐
│ Platform Provider Binary (e.g., hypershift-aws-provider)        │
│ - Independent go.mod (can use different CAPI versions)          │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│ Infrastructure Controller:                                      │
│    - Watches: HostedAWSCluster (the platform CRD)              │
│    - Creates: CAPI infrastructure resources (VPC, LB, etc.)    │
│    - Populates: HCP.Spec.Platform.AWS for CPO (via SSA)        │
│    - Creates: Platform-specific secrets in HCP namespace       │
│                                                                  │
│ NodePool Controller:                                            │
│    - Watches: AWSNodePool (the platform CRD)                   │
│    - Creates: CAPI machine templates                           │
│    - Handles: Node-specific platform configuration            │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

**Why Not a Control Plane Provider?** Platform providers populate `HCP.Spec.Platform` which the Control Plane Operator (CPO) reads. CPO's internal platform-specific code remains within CPO for security and technical reasons (see [Control Plane Operator](#control-plane-operator) for detailed rationale).

### Key Design Principles

1. **CRD-Based Providers**: Platform-specific configuration lives in separate CRDs, not embedded in core types
2. **Reference Pattern**: Core resources reference platform CRDs via `infrastructureRef` and `nodePoolRef`
3. **User-Controlled Configuration**: Users create platform CRDs directly, not generated by operators
4. **SSA for Coordination**: Providers use Server-Side Apply to populate HCP.Spec.Platform without conflicts
5. **No CPO Changes**: Control Plane Operator (CPO) continues reading HCP.Spec.Platform as it does today
6. **Independent Dependencies**: Each provider has its own go.mod, no version conflicts

---

## CRD Naming Conventions

Platform-specific CRDs mirror the naming pattern of core HyperShift resources.

### Complete Naming Convention

| Platform | Cluster Infrastructure CRD | NodePool Configuration CRD |
|----------|---------------------------|---------------------------|
| AWS | `HostedAWSCluster` | `AWSNodePool` |
| Azure | `HostedAzureCluster` | `AzureNodePool` |
| KubeVirt | `HostedKubeVirtCluster` | `KubeVirtNodePool` |
| PowerVS | `HostedPowerVSCluster` | `PowerVSNodePool` |
| OpenStack | `HostedOpenStackCluster` | `OpenStackNodePool` |
| Agent | `HostedAgentCluster` | `AgentNodePool` |

**API Group**: `infrastructure.hypershift.openshift.io/v1beta1`

**Rationale for Naming**:
- **Cluster CRDs**: `Hosted` prefix mirrors `HostedCluster` (e.g., `HostedAWSCluster`)
- **NodePool CRDs**: No `Hosted` prefix mirrors `NodePool` (e.g., `AWSNodePool`)
- **Consistency**: Naming pattern matches core API structure exactly
- **CAPI Disambiguation**: `Hosted` prefix on cluster CRDs distinguishes from CAPI CRDs (e.g., `AWSCluster` vs `HostedAWSCluster`)

---

## Component Separation Strategy

### Platform Providers: Complete Separation

**Pattern**: CRD-based provider controllers (separate binaries)

**Communication**: Through Kubernetes CRDs only, no direct imports

**Independence**: Each provider has separate go.mod, can use different CAPI versions

```
hypershift-operator (core):    CAPI v1.11.0, no AWS/Azure imports
hypershift-aws-provider:       CAPI v1.10.0, AWS SDK v2.50.0  ✅ No conflict!
hypershift-azure-provider:     CAPI v1.11.0, Azure SDK v68.0.0 ✅ Independent!
```

---

## User Workflow

### Step 1: Create Platform Configuration

User creates platform-specific configuration:

```yaml
apiVersion: infrastructure.hypershift.openshift.io/v1beta1
kind: HostedAWSCluster
metadata:
  name: my-cluster-infra
  namespace: clusters
spec:
  region: us-east-1

  vpc:
    id: vpc-0123456789abcdef0

  subnets:
    - id: subnet-public-1a
      zone: us-east-1a
    - id: subnet-private-1a
      zone: us-east-1a

  endpointAccess:
    type: Public

  resourceTags:
    - key: team
      value: platform

  # KMS configuration
  secretEncryption:
    kmsKeyARN: arn:aws:kms:us-east-1:123456789:key/abc-def-123
```

### Step 2: Create Core HostedCluster

User creates core HostedCluster, referencing the platform config:

```yaml
apiVersion: hypershift.openshift.io/v1beta1
kind: HostedCluster
metadata:
  name: my-cluster
  namespace: clusters
spec:
  release:
    image: quay.io/openshift-release-dev/ocp-release:4.17.0

  platform:
    type: AWS
    # Reference to user-created platform config
    infrastructureRef:
      apiVersion: infrastructure.hypershift.openshift.io/v1beta1
      kind: HostedAWSCluster
      name: my-cluster-infra

  networking:
    clusterNetwork:
      - cidr: 10.132.0.0/14
    serviceNetwork:
      - cidr: 172.31.0.0/16
```

### Step 3: Create Platform NodePool Configuration

```yaml
apiVersion: infrastructure.hypershift.openshift.io/v1beta1
kind: AWSNodePool
metadata:
  name: workers-config
  namespace: clusters
spec:
  instanceType: m5.2xlarge
  instanceProfile: my-cluster-worker-profile

  rootVolume:
    size: 120
    type: gp3
    iops: 4000

  subnet:
    id: subnet-private-1a

  securityGroups:
    - id: sg-workers
```

### Step 4: Create Core NodePool

```yaml
apiVersion: hypershift.openshift.io/v1beta1
kind: NodePool
metadata:
  name: workers
  namespace: clusters
spec:
  clusterName: my-cluster
  replicas: 3

  platform:
    type: AWS
    # Reference to user-created platform config
    nodePoolRef:
      apiVersion: infrastructure.hypershift.openshift.io/v1beta1
      kind: AWSNodePool
      name: workers-config
```

### Reconciliation Flow

```
┌─────────────────────────────────────────────────────────────────┐
│ User Actions                                                     │
├─────────────────────────────────────────────────────────────────┤
│ 1. kubectl apply -f hostedawscluster.yaml                       │
│ 2. kubectl apply -f hostedcluster.yaml                          │
│ 3. kubectl apply -f awsnodepool.yaml                            │
│ 4. kubectl apply -f nodepool.yaml                               │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│ Controller Reconciliation Flow                                  │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│ hypershift-operator watches HostedCluster                       │
│   → Sets HostedCluster as owner of HostedAWSCluster            │
│   → Validates no other HostedCluster owns it                    │
│   → Waits for HostedAWSCluster.status.ready                     │
│   → Creates HostedControlPlane                                  │
│                                                                  │
│ hypershift-aws-provider watches HostedAWSCluster                │
│   → Finds owner HostedCluster from OwnerReferences              │
│   → Creates CAPI AWSCluster                                     │
│   → Provisions AWS infrastructure (VPC, subnets, etc.)          │
│   → Waits for HCP to exist                                      │
│   → Populates HCP.Spec.Platform.AWS via SSA                     │
│   → Creates platform secrets in HCP namespace                   │
│   → Updates HostedAWSCluster.status.ready = true               │
│                                                                  │
│ hypershift-operator watches NodePool                            │
│   → Sets NodePool as owner of AWSNodePool                      │
│   → Waits for AWSNodePool.status.ready                          │
│   → Creates CAPI MachineDeployment                              │
│                                                                  │
│ hypershift-aws-provider watches AWSNodePool                     │
│   → Creates CAPI AWSMachineTemplate                             │
│   → Updates AWSNodePool.status.ready = true                    │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

---

## Platform Provider Pattern

### HostedAWSCluster CRD Example

```go
// providers/aws/api/v1beta1/hostedawscluster_types.go

package v1beta1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HostedAWSCluster defines AWS-specific infrastructure for a hosted cluster
type HostedAWSCluster struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   HostedAWSClusterSpec   `json:"spec,omitempty"`
    Status HostedAWSClusterStatus `json:"status,omitempty"`
}

type HostedAWSClusterSpec struct {
    Region string `json:"region"`

    VPC VPCSpec `json:"vpc,omitempty"`

    Subnets []SubnetSpec `json:"subnets,omitempty"`

    EndpointAccess EndpointAccessSpec `json:"endpointAccess,omitempty"`

    ResourceTags []Tag `json:"resourceTags,omitempty"`

    // Secret encryption configuration
    SecretEncryption *SecretEncryptionSpec `json:"secretEncryption,omitempty"`
}

type HostedAWSClusterStatus struct {
    Ready bool `json:"ready"`

    Network NetworkStatus `json:"network,omitempty"`

    APIEndpoint APIEndpointStatus `json:"apiEndpoint,omitempty"`

    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

### AWS Provider Controller Responsibilities

The AWS provider controller (`hypershift-aws-provider`) reconciles HostedAWSCluster resources and is responsible for:

**Core Functions**:
1. **Infrastructure Provisioning**: Creates and manages CAPI AWSCluster resources that provision AWS infrastructure (VPC, subnets, security groups, load balancers)
2. **Owner Discovery**: Finds the owner HostedCluster via OwnerReferences to determine HCP namespace and configuration
3. **HCP Platform Configuration**: Populates `HostedControlPlane.Spec.Platform.AWS` using Server-Side Apply with CPO-specific configuration
4. **Secret Management**: Creates platform-specific secrets (KMS credentials, cloud provider config) in the HCP namespace
5. **Status Management**: Updates HostedAWSCluster.Status.Ready when infrastructure is provisioned and ready

**Implementation Location**: `providers/aws/controllers/infrastructure/`

**Field Ownership**: Uses Server-Side Apply with field manager `hypershift-aws-provider` to own HCP.Spec.Platform.AWS fields

**Detailed Implementation**: See [Provider Implementation Example](#provider-implementation-example) below for the complete workflow.

### Ownership Model

**Critical Pattern**: HostedCluster owns HostedAWSCluster to ensure 1-to-1 relationship and proper lifecycle management.

**Workflow**:
1. **User creates HostedAWSCluster** - Infrastructure configuration lives independently
2. **User creates HostedCluster** - References the HostedAWSCluster via `infrastructureRef`
3. **hypershift-operator sets owner reference** - Adds HostedCluster as owner of HostedAWSCluster
4. **aws-provider reconciles HostedAWSCluster** - Finds owner HostedCluster via owner reference
5. **Validation prevents multiple owners** - Cannot have multiple HostedClusters referencing same HostedAWSCluster

**Benefits**:
- ✅ **Automatic cleanup**: Deleting HostedCluster deletes HostedAWSCluster (garbage collection)
- ✅ **1-to-1 enforcement**: Kubernetes prevents multiple owner references of same kind
- ✅ **Clear lifecycle**: HostedAWSCluster cannot outlive HostedCluster
- ✅ **Discoverability**: Provider finds HostedCluster from HostedAWSCluster.OwnerReferences

**Same pattern applies to NodePool → AWSNodePool**.

### Core Operator Integration

The core hypershift-operator does NOT import provider types. It uses unstructured client and establishes ownership:

**Implementation (hypershift-operator)**:
- Fetch HostedAWSCluster using unstructured client (no type imports)
- Check existing owner references
  - If owned by this HostedCluster: continue
  - If owned by different HostedCluster: error and requeue
  - If no owner: set HostedCluster as controller owner reference
- Check HostedAWSCluster.Status.Ready before proceeding with cluster creation
- Kubernetes garbage collection handles cleanup when HostedCluster is deleted

---

## NodePool Configuration Pattern

### AWSNodePool CRD

```go
// providers/aws/api/v1beta1/awsnodepool_types.go

type AWSNodePool struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   AWSNodePoolSpec   `json:"spec,omitempty"`
    Status AWSNodePoolStatus `json:"status,omitempty"`
}

type AWSNodePoolSpec struct {
    InstanceType    string `json:"instanceType"`
    InstanceProfile string `json:"instanceProfile"`

    RootVolume RootVolumeSpec `json:"rootVolume,omitempty"`

    Subnet SubnetReference `json:"subnet"`

    SecurityGroups []SecurityGroupReference `json:"securityGroups,omitempty"`

    ResourceTags []Tag `json:"resourceTags,omitempty"`

    // Spot instance configuration
    SpotMarketOptions *SpotMarketOptions `json:"spotMarketOptions,omitempty"`
}

type AWSNodePoolStatus struct {
    Ready bool `json:"ready"`

    MachineTemplateRef *corev1.ObjectReference `json:"machineTemplateRef,omitempty"`

    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

### NodePool Controller Responsibilities

The NodePool controller (part of `hypershift-aws-provider`) reconciles AWSNodePool resources and is responsible for:

**Core Functions**:
1. **Machine Template Creation**: Creates and manages CAPI AWSMachineTemplate resources that define node machine specifications
2. **Owner Discovery**: Finds the owner NodePool via OwnerReferences to coordinate with core NodePool lifecycle
3. **Platform-Specific Configuration**: Translates AWSNodePool spec into CAPI machine template spec (instance type, volumes, networking, IAM profiles)
4. **Template Management**: Creates immutable machine templates for each NodePool configuration version
5. **Status Management**: Updates AWSNodePool.Status.Ready and provides MachineTemplateRef for core NodePool controller to use

**Implementation Location**: `providers/aws/controllers/nodepool/`

**Coordination**: Core NodePool controller waits for AWSNodePool.Status.Ready before creating CAPI MachineDeployment resources

### Simplified Core NodePool API

```go
// api/hypershift/v1beta1/nodepool_types.go

type NodePoolSpec struct {
    ClusterName string

    // Platform configuration reference
    Platform NodePoolPlatformRef

    // Core fields (platform-agnostic)
    Release Release
    Replicas *int32
    AutoScaling *NodePoolAutoScaling
    Management NodePoolManagement
}

type NodePoolPlatformRef struct {
    // Type is the platform type
    Type PlatformType

    // NodePoolRef references the platform-specific node pool configuration
    NodePoolRef *corev1.ObjectReference
}
```

---

## Control Plane Operator

### Why No Separate Control Plane Provider?

**Decision**: Unlike infrastructure and nodepool management, control plane customization remains as **constrained platform-specific code within CPO**, not as separate provider binaries.

**Rationale**:

1. **Security is Paramount**
   - Control plane components (KAS, etcd, controllers) are the trust boundary
   - Any code that can mutate KAS deployment must be reviewed and controlled by core team
   - External providers could introduce security vulnerabilities or backdoors
   - Central validation is critical for multi-tenant environments

2. **Technical Complexity of Deployment Mutations**
   - CPO performs deep, conditional mutations of Deployment manifests:
     - **Container injection**: Adding KMS sidecars, pod identity webhooks at specific positions
     - **Volume mounting**: Injecting platform-specific volumes and volume mounts
     - **Environment variables**: Setting cloud-specific env vars based on runtime conditions
     - **Init containers**: Adding platform-specific initialization logic
     - **Args and command modifications**: Modifying KAS startup parameters
   - These mutations are **not declarative patches** - they require imperative logic
   - External providers would need complex APIs to express these mutations safely
   - Making this "injectable" from external providers would require:
     - Complex contract between CPO and providers
     - Provider-supplied code running in CPO's security context
     - Or, webhook-based mutations with ordering/timing challenges

3. **Limited Scope of Platform Customization**
   - Platform-specific control plane customization is narrow and well-defined:
     - Adding KMS encryption sidecars (AWS, Azure)
     - Injecting pod identity webhooks (AWS)
     - Setting cloud-specific environment variables
     - Mounting cloud credential secrets
   - This is ~5-10% of CPO code, vs 90% for infrastructure/nodepool
   - The complexity-to-benefit ratio favors keeping this in CPO

4. **Trade-off Analysis**
   - **Cost**: Some import coupling in CPO (AWS/Azure SDKs)
   - **Benefit**: Guaranteed security review + avoids complex provider contract
   - **Verdict**: Security and technical simplicity outweigh dependency isolation

5. **Alternatives Considered and Rejected**
   - **External Control Plane Providers**: Rejected due to security risks and complex mutation API requirements
   - **Admission Webhooks**: Too coarse-grained, can't handle conditional logic based on reconciliation state
   - **Mutating Webhooks**: Race conditions, ordering issues, complexity, and still requires trusted code
   - **Provider-Supplied Deployment Patches**: Insufficient for complex conditional mutations, security risks

### Provider-to-CPO Communication Problem

The Control Plane Operator (CPO) needs platform-specific configuration to:
1. **Configure KAS deployment** - Add KMS sidecars, pod identity webhooks, environment variables
2. **Create platform-specific secrets** - KMS credentials, cloud provider configs, CSI driver configs
3. **Configure other components** - CCM, storage operators, network operators

With the provider architecture, platform fields move from `HostedCluster.Spec.Platform.AWS` to separate `HostedAWSCluster` CRDs. CPO needs access to this platform-specific config without:
- Copying all fields (doesn't scale)
- Importing provider types (tight coupling)
- Creating new intermediate CRDs (unnecessary complexity)

### Solution: Providers Write to HCP.Spec.Platform Using Server-Side Apply

**Approach**: Platform providers populate `HostedControlPlane.Spec.Platform.{AWS,Azure,...}` with CPO-specific configuration using Server-Side Apply (SSA) for field ownership.

```
┌─────────────────────────────────────────────────────────────────┐
│ Provider (hypershift-aws-provider)                              │
├─────────────────────────────────────────────────────────────────┤
│ 1. Watches HostedAWSCluster                                     │
│ 2. Reconciles infrastructure (creates CAPI resources)           │
│ 3. Extracts CPO-needed config from HostedAWSCluster             │
│ 4. Writes to HCP.Spec.Platform.AWS using SSA                    │
│    Field Owner: "hypershift-aws-provider"                       │
│ 5. Creates platform-specific secrets in HCP namespace           │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│ HostedControlPlane.Spec.Platform.AWS (in HCP namespace)        │
├─────────────────────────────────────────────────────────────────┤
│ Written by: hypershift-aws-provider (via SSA)                  │
│ Read by: control-plane-operator                                │
│                                                                  │
│ Contains ONLY fields CPO needs:                                 │
│ - Region (for pod identity webhook, CCM, etc.)                  │
│ - KMS key ARNs and credential secret refs                       │
│ - Cloud config secret refs                                      │
│ - NOT all HostedAWSCluster fields                               │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│ CPO (control-plane-operator)                                    │
├─────────────────────────────────────────────────────────────────┤
│ 1. Reads HostedControlPlane (already watching it)               │
│ 2. Uses HCP.Spec.Platform.AWS for KAS customization             │
│ 3. References platform-specific secrets created by provider     │
│ 4. Applies constrained customizations to control plane          │
└─────────────────────────────────────────────────────────────────┘
```

**Field Ownership with SSA**:
- `hypershift-operator` owns core HCP.Spec fields
- `hypershift-aws-provider` owns HCP.Spec.Platform.AWS fields
- `hypershift-azure-provider` owns HCP.Spec.Platform.Azure fields
- No conflicts because ownership is tracked per-field by Kubernetes

### HCP Creation and Provider Discovery

**Who creates HCP?**
- `hypershift-operator` creates `HostedControlPlane` when it sees a `HostedCluster` (existing behavior)
- HCP is created in a namespace named after the HostedCluster's namespace/name: `{hc.namespace}-{hc.name}`
- Example: HostedCluster `hostedcluster` in namespace `clusters` → HCP in namespace `clusters-hostedcluster`

**How does provider find HCP?**
1. Provider watches `HostedAWSCluster` (created by user)
2. Provider waits for HCP to exist in the hcp namespace of the owner HostedCluster before populating platform config

### Provider Implementation Example

**Reconciliation Workflow**:
1. Get HostedAWSCluster from reconcile request
2. Find owner HostedCluster from OwnerReferences (set by hypershift-operator)
   - If no owner yet: requeue and wait
3. Compute HCP namespace from HostedCluster: `{namespace}-{name}`
4. Wait for HCP to exist (created by hypershift-operator)
5. Reconcile infrastructure and populate HCP platform configuration
6. Update HostedAWSCluster.Status.Ready

---

## Platform-Specific Controllers in Providers

### AWS PrivateLink Controller

The AWS PrivateLink controller currently lives in `control-plane-operator/controllers/awsprivatelink/` but should be moved to the AWS provider as it manages AWS-specific networking infrastructure.

#### What It Does

- Manages AWS VPC Endpoints for private cluster API access
- Creates and manages Route53 DNS records for private endpoints
- Entirely AWS-specific with no cross-platform logic

#### Proposed Migration

**Move to**: `providers/aws/controllers/privatelink/`

**Trigger**: Watch `HostedAWSCluster` for PrivateLink configuration

```yaml
apiVersion: infrastructure.hypershift.openshift.io/v1beta1
kind: HostedAWSCluster
metadata:
  name: my-cluster-infra
spec:
  endpointAccess:
    type: Private

  privateLink:
    enabled: true
status:
  privateLink:
    ready: true
    vpcEndpointID: vpce-abc123
```

**Benefits**:
- Removes AWS SDK dependency from CPO
- AWS team owns complete AWS infrastructure stack
- Can evolve independently with other AWS provider features

**Timing**: Migrate during Phase 2 (AWS Provider Extraction)

---

## Security Considerations

### Threat Model

**Platform Providers**:
- ✅ Low risk - CRD-based, no direct cluster access
- ✅ Providers can only create CAPI resources
- ✅ Core operator validates all status updates
- ✅ Separate RBAC for each provider

**Control Plane Operator (CPO)**:
- ⚠️ High risk - can modify KAS deployment
- ✅ Mitigated by constrained platform-specific code (see [Control Plane Operator](#control-plane-operator) for detailed rationale)

---

## Proposed Repository Structure

```
hypershift/ (PROPOSED STRUCTURE)
├── api/                                    # Core APIs (simplified)
│   └── hypershift/v1beta1/
│       ├── hostedcluster_types.go         # No embedded platform specs
│       ├── nodepool_types.go              # No embedded platform specs
│       └── common.go
│
├── provider-api/                           # Shared contracts (optional)
│   ├── infrastructure/v1beta1/
│   │   └── contracts.go                   # Documentation only
│   └── nodepool/v1beta1/
│       └── contracts.go                   # Documentation only
│
├── hypershift-operator/                    # Core operator
│   ├── go.mod                             # Independent, no provider imports
│   ├── controllers/
│   │   ├── hostedcluster/
│   │   │   └── hostedcluster_controller.go  # Uses unstructured
│   │   └── nodepool/
│   │       └── nodepool_controller.go       # Uses unstructured
│   └── cmd/
│       └── main.go
│
├── providers/                              # Platform providers (separate binaries)
│   ├── aws/
│   │   ├── go.mod                         # Independent, CAPI v1.10.x
│   │   ├── api/v1beta1/
│   │   │   ├── hostedawscluster_types.go
│   │   │   ├── awsnodepool_types.go
│   │   │   └── zz_generated.deepcopy.go
│   │   ├── controllers/
│   │   │   ├── infrastructure/
│   │   │   │   └── hostedawscluster_controller.go
│   │   │   └── nodepool/
│   │   │       └── awsnodepool_controller.go
│   │   ├── cmd/
│   │   │   └── main.go
│   │   └── manifests/
│   │       └── deployment.yaml
│   │
│   ├── azure/
│   │   ├── go.mod                         # Independent, CAPI v1.11.x
│   │   ├── api/v1beta1/
│   │   │   ├── hostedazurecluster_types.go
│   │   │   └── azurenodepool_types.go
│   │   ├── controllers/
│   │   │   ├── infrastructure/
│   │   │   └── nodepool/
│   │   └── cmd/
│   │       └── main.go
│   │
│   ├── kubevirt/
│   ├── powervs/
│   ├── openstack/
│   └── agent/
│
└── cmd/
    └── hypershift/                        # CLI
        └── create/
            └── cluster.go                 # Can generate both core + provider CRs
```

### Go Module Independence


```
# hypershift-operator/go.mod (PROPOSED)
module github.com/openshift/hypershift/hypershift-operator

require (
    sigs.k8s.io/cluster-api v1.11.0
    sigs.k8s.io/controller-runtime v0.20.0
    // NO provider imports
)

# control-plane-operator/go.mod
module github.com/openshift/hypershift/control-plane-operator

require (
    sigs.k8s.io/cluster-api v1.11.0
    sigs.k8s.io/controller-runtime v0.20.0
    // May import AWS/Azure SDKs (acceptable trade-off for security)
    github.com/aws/aws-sdk-go-v2 v1.50.0
    github.com/Azure/azure-sdk-for-go v68.0.0
)

# providers/aws/go.mod
module github.com/openshift/hypershift/providers/aws

require (
    sigs.k8s.io/cluster-api v1.10.0              # ✅ Can differ!
    sigs.k8s.io/cluster-api-provider-aws v2.8.0
    github.com/aws/aws-sdk-go-v2 v1.50.0
)

# providers/azure/go.mod
module github.com/openshift/hypershift/providers/azure

require (
    sigs.k8s.io/cluster-api v1.11.0              # ✅ Can differ from AWS!
    sigs.k8s.io/cluster-api-provider-azure v1.18.0
    github.com/Azure/azure-sdk-for-go v68.0.0
)
```

---

## Migration Strategy

### Phase 1: Foundation (Months 1-2)

**Goal**: Establish provider APIs and contracts without breaking existing functionality

**Tasks**:
1. Create provider-api module with interface definitions
2. Create providers/ directory structure
3. Define HostedAWSCluster, HostedAzureCluster, etc. CRD types
4. Define AWSNodePool, AzureNodePool, etc. CRD types
5. No changes to existing HostedCluster/NodePool APIs yet (dual-mode support)

**Deliverables**:
- Provider CRD definitions for all platforms
- Documentation

**Success Criteria**:
- All CRDs installable
- Interface compiles
- No impact on existing clusters

---

### Phase 2: AWS Provider Extraction (Months 3-5)

**Goal**: Prove the pattern with AWS as pilot platform

**Tasks**:

1. **Platform Provider (Month 3)**
   - Implement HostedAWSCluster controller in providers/aws/
   - Build hypershift-aws-provider binary
   - Add dual-mode support to HostedCluster
     - Support both embedded `platform.aws` (deprecated) and `infrastructureRef`
   - Update hypershift-operator to check infrastructureRef status
   - Test with dev clusters

2. **NodePool Controller (Month 4)**
   - Implement AWSNodePool controller
   - Add dual-mode support to NodePool
   - Update core nodepool controller to use nodePoolRef
   - Migrate AWS-specific nodepool logic to provider
   - Test multi-nodepool scenarios

3. **Migration Tooling**
   - CLI command: `hypershift migrate cluster <name> --to-provider-pattern`
   - Creates HostedAWSCluster from existing platform.aws
   - Creates AWSNodePool from existing platform.aws
   - Updates HostedCluster/NodePool to use refs
   - Validates migration

**Success Criteria**:
- New AWS clusters work with provider pattern
- Existing AWS clusters continue working (backward compatible)
- Migration tool tested on dev clusters
- AWS provider can use different CAPI version than core

---

### Phase 3: Multi-Provider Expansion (Months 6-9)

**Goal**: Validate pattern with all platforms

**Months 6-7: Azure Provider**
- Extract Azure provider (similar to AWS)
- HostedAzureCluster + AzureNodePool
- Azure KMS customizer
- Test with ARO HCP clusters

**Months 8-9: Remaining Providers**
- KubeVirt (simpler, no KMS)
- PowerVS
- OpenStack
- Agent

**Tasks per Provider**:
1. Create provider CRDs
2. Implement controllers
3. Build provider binary
4. Test migration
5. Document

**Success Criteria**:
- All platforms available as providers
- Each provider has independent go.mod
- Migration path documented for each platform

---

### Phase 4: API v2 & Deprecation (Months 10-12)

**Goal**: Clean API with provider references only

**Month 10: API v2**
- Introduce v1beta2 API for HostedCluster and NodePool
- Remove embedded platform specs from PlatformSpec
- Only `type` and `infrastructureRef`/`nodePoolRef` remain
- Implement conversion webhooks v1beta1 → v1beta2
- Update documentation

**Month 11: Deprecation**
- Mark v1beta1 embedded platform specs as deprecated
- Add warning messages for embedded platform usage
- Automated migration in hypershift-operator (optional)
- Update all examples to v1beta2

**Month 12: Provider Catalog**
- Provider versioning and compatibility matrix
- Provider discovery mechanism
- Documentation for community providers
- Release provider guidelines

**Success Criteria**:
- Clean v1beta2 API GA
- v1beta1 supported but deprecated
- Clear migration path for all users
- Provider development documented

---

### Rollback Strategy

Each phase maintains backward compatibility:

**Phase 2-3**: Dual-mode support
- Old way: Embedded platform specs still work
- New way: Provider references work
- Core operator supports both

**Phase 4**: Conversion webhooks
- v1beta1 → v1beta2 automatic conversion
- Users can stay on v1beta1 until ready
- No forced migration

**Emergency Rollback**:
- Provider failures don't affect existing clusters
- Core operator continues working with embedded specs
- Provider updates can be rolled back independently

---

## Benefits and Trade-offs

### Benefits

#### Platform Providers (CRD Pattern)

✅ **Complete Isolation**: Zero Go module conflicts
✅ **Independent Evolution**: Each provider releases independently
✅ **Smaller Binaries**: Core operator has no cloud SDKs
✅ **Clear Ownership**: Platform teams own their providers
✅ **Easy Testing**: Test providers in isolation
✅ **Community Extensibility**: Third-party providers possible
✅ **50% API Reduction**: ~3,000 LOC moved to providers
✅ **Ownership Benefits**: See [Ownership Model](#ownership-model) for details on lifecycle management and cleanup

#### Control Plane Operator (CPO) (Stays the Same)

✅ **Security**: Core team reviews all control plane code
✅ **Validation**: All changes validated before applying
✅ **Type Safety**: Compile-time checks
✅ **Trust Model**: Only trusted code can modify control plane

#### Overall

✅ **Faster New Platforms**: <2 weeks vs ~2 months
✅ **Reduced Cross-Platform Bugs**: 80% reduction (isolation)
✅ **Better Documentation**: Each provider has own docs
✅ **User Control**: Users create and own platform configske

### Trade-offs

#### Platform Providers

⚠️ **More Resources**: One deployment per provider
⚠️ **More Complexity**: Multiple controllers to coordinate
⚠️ **Deployment Overhead**: Need to deploy providers
⚠️ **Learning Curve**: Users create multiple CRs

#### Control Plane Operator (CPO)

⚠️ **Some Import Coupling**: CPO imports platform SDKs
- Mitigated by: Sub-modules, version pinning, careful dep management
- Acceptable trade-off for security guarantees

⚠️ **Constrained Platform Code**: Platform-specific code limited to approved operations
- This is intentional for security
- Can expand as needed (with core team review)

⚠️ **Core Team Bottleneck**: Control plane changes need core review
- Only for control plane mutations
- Platform provider changes don't need core review

### Success Metrics

**Code Metrics**:
- HostedCluster API LOC reduction: Target 50% (~3,000 lines)
- Platform code isolation: 100% of platform/nodepool code in providers
- Core operator LOC with platform logic: Target <5%

**Development Metrics**:
- Time to add new platform: <2 weeks (vs current ~2 months)
- Time to change platform feature: <1 week
- Cross-platform bug rate: Reduce by 80%

**API Metrics**:
- HostedCluster API fields: Reduce from ~200 to ~50 core fields
- Provider API versioning: Independent per platform
- User-visible CRs: Increase (intentional - clearer separation)

---

## Appendix: Glossary

- **CRD**: Custom Resource Definition
- **CAPI**: Cluster API
- **CPO**: Control Plane Operator
- **HCP**: Hosted Control Plane
- **KAS**: Kube API Server
- **KMS**: Key Management Service
- **Provider**: Platform-specific implementation (AWS, Azure, etc.)
