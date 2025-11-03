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

1. [Current State Analysis](#current-state-analysis)
2. [Architecture Overview](#architecture-overview)
3. [CRD Naming Conventions](#crd-naming-conventions)
4. [Component Separation Strategy](#component-separation-strategy)
5. [User Workflow](#user-workflow)
6. [Platform Provider Pattern](#platform-provider-pattern)
7. [NodePool Provider Pattern](#nodepool-provider-pattern)
8. [Control Plane Operator](#control-plane-operator)
9. [Security Considerations](#security-considerations)
10. [Repository Structure](#repository-structure)
11. [Migration Strategy](#migration-strategy)
12. [Benefits and Trade-offs](#benefits-and-trade-offs)

---

## Current State Analysis

### API Complexity

**Total API Code**: ~10,744 lines across all files

**Platform-Specific API Code**: ~2,976 lines (28% of total)
- AWS: 1,004 lines
- Azure: 774 lines
- OpenStack: 451 lines
- KubeVirt: 403 lines
- PowerVS: 324 lines
- Agent: 20 lines

### Current Issues

1. **Monolithic API**: All platform specs embedded in single `PlatformSpec` struct
2. **Tight Coupling**: Platform logic mixed throughout hypershift-operator and control-plane-operator
3. **Switch-Based Dispatch**: Central `GetPlatform()` function requires modification for each new platform
4. **Growing Complexity**: 34+ platform-specific files in control-plane-operator alone
5. **Cross-Platform Risk**: Changes to one platform can affect others
6. **Import Conflicts**: Risk of Go module dependency conflicts as providers use different versions

---

## Architecture Overview

### Two-Tier Provider Pattern

```
┌─────────────────────────────────────────────────────────────────┐
│ 1. Platform Provider (CRD-based)                                │
│    - Manages HostedAWSCluster, HostedAzureCluster, etc.        │
│    - Separate binary: hypershift-aws-provider                   │
│    - Independent go.mod (can use CAPI 1.10.x)                   │
│    - Creates CAPI infrastructure resources                      │
│    - Populates HCP.Spec.Platform.AWS for CPO (via SSA)          │
│    - Creates platform-specific secrets in HCP namespace         │
│    - Updates status.ready when infrastructure is provisioned    │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│ 2. NodePool Provider (CRD-based)                                │
│    - Manages HostedAWSNodePool, HostedAzureNodePool, etc.      │
│    - Same binary as platform provider                           │
│    - Creates CAPI machine templates                             │
│    - Handles node-specific platform configuration               │
└─────────────────────────────────────────────────────────────────┘
```

**No separate Control Plane Provider binary**: Platform providers populate `HCP.Spec.Platform` which the Control Plane Operator (CPO) reads. CPO's internal platform-specific code remains unchanged for security reasons (see [Control Plane Operator](#control-plane-operator) for detailed rationale).

### Key Design Principles

1. **CRD-Based Providers**: Providers are separate controllers watching their own CRDs
2. **User-Controlled Configuration**: Users create platform CRDs directly, not core operator
3. **SSA for Coordination**: Providers use Server-Side Apply to populate HCP.Spec.Platform without conflicts
4. **No CPO Changes**: Control Plane Operator (CPO) continues reading HCP.Spec.Platform as it does today
5. **Independent Dependencies**: Each provider has its own go.mod, no version conflicts

---

## CRD Naming Conventions

All platform-specific CRDs use the `Hosted` prefix to avoid conflicts with Cluster API provider CRDs.

### Complete Naming Convention

| Platform | Platform CRD | NodePool Config CRD |
|----------|--------------|---------------------|
| AWS | `HostedAWSCluster` | `HostedAWSNodePool` |
| Azure | `HostedAzureCluster` | `HostedAzureNodePool` |
| KubeVirt | `HostedKubeVirtCluster` | `HostedKubeVirtNodePool` |
| PowerVS | `HostedPowerVSCluster` | `HostedPowerVSNodePool` |
| OpenStack | `HostedOpenStackCluster` | `HostedOpenStackNodePool` |
| Agent | `HostedAgentCluster` | `HostedAgentNodePool` |

**API Group**: `infrastructure.hypershift.openshift.io/v1beta1`

**Rationale for Naming**:
- `Hosted` prefix clearly distinguishes from CAPI CRDs (e.g., `AWSCluster`)
- Consistent with existing HyperShift naming: `HostedCluster`, `HostedControlPlane`
- Better grouping when listing resources: `kubectl get hosted*`

---

## Component Separation Strategy

### Platform & NodePool Providers: Complete Separation

**Pattern**: CRD-based provider controllers (separate binaries)

**Communication**: Through Kubernetes CRDs only, no direct imports

**Independence**: Each provider has separate go.mod, can use different CAPI versions

```
hypershift-operator (core):    CAPI v1.11.0, no AWS/Azure imports
hypershift-aws-provider:       CAPI v1.10.0, AWS SDK v2.50.0  ✅ No conflict!
hypershift-azure-provider:     CAPI v1.11.0, Azure SDK v68.0.0 ✅ Independent!
```

### Control Plane Operator (CPO): Constrained Code in Core

**Rationale**: Security - core team must control all control plane mutations

**Trade-off**: Accept some import coupling in CPO for security guarantees

**Mitigation**:
- Constrained interface limits what providers can do
- Core validates all provider changes before applying
- Platform code isolated in separate packages
- Consider sub-modules with replace directives for SDK isolation

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
kind: HostedAWSNodePool
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
      kind: HostedAWSNodePool
      name: workers-config
```

### Reconciliation Flow

```
┌─────────────────────────────────────────────────────────────────┐
│ User Actions                                                     │
├─────────────────────────────────────────────────────────────────┤
│ 1. kubectl apply -f hostedawscluster.yaml                       │
│ 2. kubectl apply -f hostedcluster.yaml                          │
│ 3. kubectl apply -f hostedawsnodepool.yaml                      │
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
│   → Sets NodePool as owner of HostedAWSNodePool                │
│   → Validates no other NodePool owns it                         │
│   → Waits for HostedAWSNodePool.status.ready                    │
│   → Creates CAPI MachineDeployment                              │
│                                                                  │
│ hypershift-aws-provider watches HostedAWSNodePool               │
│   → Finds owner NodePool from OwnerReferences                   │
│   → Creates CAPI AWSMachineTemplate                             │
│   → Updates HostedAWSNodePool.status.ready = true              │
│   → Worker nodes provisioned                                    │
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

### AWS Provider Controller

```go
// providers/aws/controllers/infrastructure/hostedawscluster_controller.go

package infrastructure

import (
    "context"

    awsinfrav1 "github.com/openshift/hypershift/providers/aws/api/v1beta1"
    capav1 "sigs.k8s.io/cluster-api-provider-aws/api/v1beta2"

    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

type HostedAWSClusterReconciler struct {
    client.Client
}

func (r *HostedAWSClusterReconciler) Reconcile(
    ctx context.Context,
    req ctrl.Request,
) (ctrl.Result, error) {
    awsCluster := &awsinfrav1.HostedAWSCluster{}
    if err := r.Get(ctx, req.NamespacedName, awsCluster); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Create/update CAPI AWSCluster
    capiCluster := &capav1.AWSCluster{
        ObjectMeta: metav1.ObjectMeta{
            Name:      awsCluster.Name,
            Namespace: awsCluster.Namespace,
        },
        Spec: capav1.AWSClusterSpec{
            Region: awsCluster.Spec.Region,
            NetworkSpec: capav1.NetworkSpec{
                VPC: capav1.VPCSpec{
                    ID: awsCluster.Spec.VPC.ID,
                },
                Subnets: convertSubnets(awsCluster.Spec.Subnets),
            },
        },
    }

    if err := r.Client.Patch(ctx, capiCluster, client.Apply,
        client.FieldOwner("hypershift-aws-provider")); err != nil {
        return ctrl.Result{}, err
    }

    // Update status based on CAPI AWSCluster status
    awsCluster.Status.Ready = capiCluster.Status.Ready
    awsCluster.Status.Network.VPCID = capiCluster.Spec.NetworkSpec.VPC.ID

    if err := r.Status().Update(ctx, awsCluster); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}
```

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

**Same pattern applies to NodePool → HostedAWSNodePool**.

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

## NodePool Provider Pattern

### HostedAWSNodePool CRD

```go
// providers/aws/api/v1beta1/hostedawsnodepool_types.go

type HostedAWSNodePool struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   HostedAWSNodePoolSpec   `json:"spec,omitempty"`
    Status HostedAWSNodePoolStatus `json:"status,omitempty"`
}

type HostedAWSNodePoolSpec struct {
    InstanceType    string `json:"instanceType"`
    InstanceProfile string `json:"instanceProfile"`

    RootVolume RootVolumeSpec `json:"rootVolume,omitempty"`

    Subnet SubnetReference `json:"subnet"`

    SecurityGroups []SecurityGroupReference `json:"securityGroups,omitempty"`

    ResourceTags []Tag `json:"resourceTags,omitempty"`

    // Spot instance configuration
    SpotMarketOptions *SpotMarketOptions `json:"spotMarketOptions,omitempty"`
}

type HostedAWSNodePoolStatus struct {
    Ready bool `json:"ready"`

    MachineTemplateRef *corev1.ObjectReference `json:"machineTemplateRef,omitempty"`

    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

### NodePool Provider Controller

```go
// providers/aws/controllers/nodepool/hostedawsnodepool_controller.go

func (r *HostedAWSNodePoolReconciler) Reconcile(
    ctx context.Context,
    req ctrl.Request,
) (ctrl.Result, error) {
    awsNodePool := &awsinfrav1.HostedAWSNodePool{}
    if err := r.Get(ctx, req.NamespacedName, awsNodePool); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Create/update CAPI AWSMachineTemplate
    machineTemplate := &capav1.AWSMachineTemplate{
        ObjectMeta: metav1.ObjectMeta{
            Name:      awsNodePool.Name + "-template",
            Namespace: awsNodePool.Namespace,
        },
        Spec: capav1.AWSMachineTemplateSpec{
            Template: capav1.AWSMachineTemplateResource{
                Spec: capav1.AWSMachineSpec{
                    InstanceType: awsNodePool.Spec.InstanceType,
                    IAMInstanceProfile: awsNodePool.Spec.InstanceProfile,
                    Subnet: &capav1.AWSResourceReference{
                        ID: awsNodePool.Spec.Subnet.ID,
                    },
                    RootVolume: &capav1.Volume{
                        Size: awsNodePool.Spec.RootVolume.Size,
                        Type: awsNodePool.Spec.RootVolume.Type,
                        IOPS: awsNodePool.Spec.RootVolume.IOPS,
                    },
                },
            },
        },
    }

    if err := r.Client.Patch(ctx, machineTemplate, client.Apply,
        client.FieldOwner("hypershift-aws-provider")); err != nil {
        return ctrl.Result{}, err
    }

    // Update status
    awsNodePool.Status.Ready = true
    awsNodePool.Status.MachineTemplateRef = &corev1.ObjectReference{
        Kind:      "AWSMachineTemplate",
        Name:      machineTemplate.Name,
        Namespace: machineTemplate.Namespace,
    }

    if err := r.Status().Update(ctx, awsNodePool); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}
```

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

2. **Limited Scope of Platform Customization**
   - Platform-specific control plane customization is narrow and well-defined:
     - Adding KMS encryption sidecars (AWS, Azure)
     - Injecting pod identity webhooks (AWS)
     - Setting cloud-specific environment variables
     - Mounting cloud credential secrets
   - This is ~5-10% of CPO code, vs 90% for infrastructure/nodepool

3. **Trade-off Analysis**
   - **Cost**: Some import coupling in CPO (AWS/Azure SDKs)
   - **Benefit**: Guaranteed security review of all control plane mutations
   - **Verdict**: Security benefits outweigh dependency isolation

4. **Alternative Considered and Rejected**
   - **External Control Plane Providers**: Rejected due to security risks
   - **Admission Webhooks**: Too coarse-grained, can't handle conditional logic
   - **Mutating Webhooks**: Race conditions, ordering issues, complexity

**Implementation**: Platform-specific control plane code stays in CPO but is:
- Isolated in separate packages (`control-plane-operator/controllers/hostedcontrolplane/kas/aws/`, etc.)
- Thoroughly reviewed by core team
- Can use sub-modules with `replace` directives to isolate SDK versions if needed

**Result**: CPO reads `HCP.Spec.Platform.{AWS,Azure,...}` (populated by platform providers via SSA) and applies validated, constrained customizations to control plane components.

### No CPO Changes Required

The current CPO implementation already reads platform-specific configuration from `HCP.Spec.Platform.{AWS,Azure,...}`. With the provider architecture:

1. **Before**: `hypershift-operator` populates `HCP.Spec.Platform.AWS` from `HostedCluster.Spec.Platform.AWS`
2. **After**: `hypershift-aws-provider` populates `HCP.Spec.Platform.AWS` from `HostedAWSCluster.Spec`

**CPO doesn't know or care about the difference** - it just reads `HCP.Spec.Platform.AWS` as always.

**What Changes**:
- ✅ Provider controllers write to `HCP.Spec.Platform` (using SSA)
- ✅ Provider controllers create platform-specific secrets in HCP namespace

**What Stays the Same**:
- ✅ CPO reads `HCP.Spec.Platform` (existing code)
- ✅ CPO KAS component uses platform config for customization (existing code)
- ✅ CPO other components use platform config (existing code)

This is the beauty of the solution - **separation without disruption**.

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

**Why This Works**:
- ✅ **No new CRD**: HCP already exists, CPO already watches it
- ✅ **Proven pattern**: Common in Kubernetes (Node.Spec, Pod.Spec, Service.Spec)
- ✅ **Simple UX**: Users only need to understand HCP, not additional CRDs
- ✅ **Atomic updates**: HCP update automatically triggers CPO reconciliation

**Field Ownership with SSA**:
- `hypershift-operator` owns core HCP.Spec fields
- `hypershift-aws-provider` owns HCP.Spec.Platform.AWS fields
- `hypershift-azure-provider` owns HCP.Spec.Platform.Azure fields
- No conflicts because ownership is tracked per-field by Kubernetes

### Platform-Specific Configuration in HCP.Spec

Providers write curated, CPO-specific configuration to `HCP.Spec.Platform.{AWS,Azure,...}`:

```go
// api/hypershift/v1beta1/platform.go

type PlatformSpec struct {
    Type PlatformType

    // AWS platform-specific control plane configuration
    // Written by: hypershift-aws-provider (via SSA)
    // Read by: control-plane-operator
    // +optional
    AWS *AWSControlPlaneConfig `json:"aws,omitempty"`

    // Azure platform-specific control plane configuration
    // Written by: hypershift-azure-provider (via SSA)
    // Read by: control-plane-operator
    // +optional
    Azure *AzureControlPlaneConfig `json:"azure,omitempty"`

    // Other platforms follow same pattern...
}

// AWSControlPlaneConfig contains AWS-specific configuration needed by CPO.
// This is a CURATED subset of HostedAWSCluster fields.
// Populated by hypershift-aws-provider based on HostedAWSCluster spec.
type AWSControlPlaneConfig struct {
    // Region is the AWS region for the control plane
    Region string `json:"region"`

    // KMS configuration for secret encryption
    // +optional
    KMS *AWSKMSControlPlaneConfig `json:"kms,omitempty"`

    // PodIdentity configuration for pod identity webhook
    // +optional
    PodIdentity *AWSPodIdentityConfig `json:"podIdentity,omitempty"`

    // CloudConfigSecretRef references the cloud provider config secret
    // Provider creates this secret in HCP namespace
    // +optional
    CloudConfigSecretRef *corev1.LocalObjectReference `json:"cloudConfigSecretRef,omitempty"`
}

type AWSKMSControlPlaneConfig struct {
    // ActiveKeyARN is the ARN of the active KMS key
    ActiveKeyARN string `json:"activeKeyARN"`

    // BackupKeyARN is the ARN of the backup KMS key
    // +optional
    BackupKeyARN string `json:"backupKeyARN,omitempty"`

    // CredentialsSecretRef references the AWS credentials secret
    // Provider creates this secret in HCP namespace
    CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

type AWSPodIdentityConfig struct {
    // Enabled indicates if pod identity webhook should be deployed
    Enabled bool `json:"enabled"`

    // KubeconfigSecretRef references the kubeconfig for the webhook
    // Provider creates this secret in HCP namespace
    // +optional
    KubeconfigSecretRef *corev1.LocalObjectReference `json:"kubeconfigSecretRef,omitempty"`
}

// Similar types for Azure, KubeVirt, PowerVS, etc.
```

**Note**: These types define what CPO needs from providers - a curated subset of platform-specific configuration, not all fields from HostedAWSCluster.

### HCP Creation and Provider Discovery

**Who creates HCP?**
- `hypershift-operator` creates `HostedControlPlane` when it sees a `HostedCluster` (existing behavior)
- HCP is created in a namespace named after the HostedCluster's namespace/name: `{hc.namespace}-{hc.name}`
- Example: HostedCluster `hostedcluster` in namespace `clusters` → HCP in namespace `clusters-hostedcluster`

**How does provider find HCP?**
1. Provider watches `HostedAWSCluster` (created by user)
2. Provider waits for HCP to exist in the hcp namespace before populating platform config

### Provider Implementation Example

**Implementation (aws-provider)**:
- Get HostedAWSCluster from reconcile request
- Find owner HostedCluster from OwnerReferences (set by hypershift-operator)
  - If no owner yet: requeue and wait
  - OwnerReference provides HostedCluster name/namespace
- Compute HCP namespace from HostedCluster: `{namespace}-{name}`
- Wait for HCP to exist (created by hypershift-operator)
- Reconcile infrastructure:
  - Create/update CAPI AWSCluster resource
  - Provision AWS infrastructure (VPC, subnets, etc.)
  - Create platform-specific secrets in HCP namespace
- Populate HCP.Spec.Platform.AWS using Server-Side Apply (SSA):
  - Field owner: `hypershift-aws-provider`
  - Only include CPO-needed fields (region, KMS config, secret refs)
  - SSA prevents conflicts with hypershift-operator
- Update HostedAWSCluster.Status.Ready when infrastructure is provisioned

**Key Points**:
- Provider owns `HCP.Spec.Platform.AWS` fields **exclusively** via SSA field manager
- `hypershift-operator` creates HCP but does NOT populate HCP.Spec.Platform
- Provider creates referenced secrets in HCP namespace
- CPO reads `HCP.Spec.Platform.AWS` as it does today - **no CPO changes needed**


### Validation and Error Handling

**Provider Responsibilities**:
- Validate HostedAWSCluster spec before writing to HCP.Spec.Platform
- Handle missing HCP gracefully (requeue until HCP exists)
- Handle missing owner reference gracefully (requeue until hypershift-operator sets it)
- Set owner references on created secrets for proper cleanup
- Update status conditions on HostedAWSCluster to indicate platform config status

**Core Operator Responsibilities**:
- Validate that referenced HostedAWSCluster exists before setting ownership
- Detect and reject attempts to reference already-owned infrastructure resources
- Set controller=true and blockOwnerDeletion=true on owner references

**CPO Responsibilities**:
- Handle missing or invalid HCP.Spec.Platform.AWS gracefully
- Log warnings if platform config is incomplete
- Continue with degraded functionality if possible (e.g., skip KMS if not configured)

**Error Scenarios**:
1. **Infrastructure resource not found**: Core operator reports error in HostedCluster status, requeues
2. **Infrastructure already owned**: Core operator rejects with clear error message
3. **HCP doesn't exist yet**: Provider requeues and waits
4. **No owner reference yet**: Provider requeues and waits for hypershift-operator
5. **Invalid platform config**: Provider reports error in HostedAWSCluster status
6. **Secret creation fails**: Provider reports error and retries
7. **SSA conflict**: Should not occur with proper field ownership; investigate if it does

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

**Platform & NodePool Providers**:
- ✅ Low risk - CRD-based, no direct cluster access
- ✅ Providers can only create CAPI resources
- ✅ Core operator validates all status updates
- ✅ Separate RBAC for each provider

**Control Plane Operator (CPO)**:
- ⚠️ High risk - can modify KAS deployment
- ⚠️ Access to control plane namespace
- ✅ Mitigated by constrained platform-specific code
- ✅ Core team validates all changes
- ✅ Only trusted code (core team reviewed)


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
│   │   │   ├── hostedawsnodepool_types.go
│   │   │   └── zz_generated.deepcopy.go
│   │   ├── controllers/
│   │   │   ├── infrastructure/
│   │   │   │   └── hostedawscluster_controller.go
│   │   │   └── nodepool/
│   │   │       └── hostedawsnodepool_controller.go
│   │   ├── cmd/
│   │   │   └── main.go
│   │   └── manifests/
│   │       └── deployment.yaml
│   │
│   ├── azure/
│   │   ├── go.mod                         # Independent, CAPI v1.11.x
│   │   ├── api/v1beta1/
│   │   │   ├── hostedazurecluster_types.go
│   │   │   └── hostedazurenodepool_types.go
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
4. Define HostedAWSNodePool, HostedAzureNodePool, etc. CRD types
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

2. **NodePool Provider (Month 4)**
   - Implement HostedAWSNodePool controller
   - Add dual-mode support to NodePool
   - Update core nodepool controller to use nodePoolRef
   - Migrate AWS-specific nodepool logic to provider
   - Test multi-nodepool scenarios

3. **Migration Tooling**
   - CLI command: `hypershift migrate cluster <name> --to-provider-pattern`
   - Creates HostedAWSCluster from existing platform.aws
   - Creates HostedAWSNodePool from existing platform.aws
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
- HostedAzureCluster + HostedAzureNodePool
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

#### Platform & NodePool Providers (CRD Pattern)

✅ **Complete Isolation**: Zero Go module conflicts
✅ **Independent Evolution**: Each provider releases independently
✅ **Smaller Binaries**: Core operator has no cloud SDKs
✅ **Clear Ownership**: Platform teams own their providers
✅ **Easy Testing**: Test providers in isolation
✅ **Community Extensibility**: Third-party providers possible
✅ **50% API Reduction**: ~3,000 LOC moved to providers
✅ **Clear Lifecycle**: Each NodePool owns its HostedAWSNodePool (1-to-1 relationship)
✅ **Enforced 1-to-1 Relationship**: Owner references prevent multiple HostedClusters from sharing platform resources
✅ **Automatic Cleanup**: Kubernetes garbage collection deletes platform resources when HostedCluster is deleted
✅ **Discoverability**: Providers easily find owner resources via OwnerReferences (no hardcoded namespace patterns)

#### Control Plane Operator (CPO) (Stays the Same)

✅ **Security**: Core team reviews all control plane code
✅ **Validation**: All changes validated before applying
✅ **Type Safety**: Compile-time checks
✅ **Trust Model**: Only trusted code can modify control plane

#### Overall

✅ **Faster New Platforms**: <2 weeks vs ~2 months
✅ **Reduced Cross-Platform Bugs**: 80% reduction (isolation)
✅ **Better Documentation**: Each provider has own docs
✅ **User Control**: Users create and own platform configs

### Trade-offs

#### Platform & NodePool Providers

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

## Conclusion

This re-architecture provides a pragmatic balance between separation, security, and maintainability:

- **90% separation**: Platform and NodePool providers via CRD-based pattern
- **10% coupling**: Control plane mutations via constrained code in CPO
- **100% security**: Core team controls all control plane changes
- **0% conflicts**: Each provider has independent dependencies

The hybrid approach acknowledges that different components have different requirements. Platform provisioning benefits from complete separation, while control plane security requires central control.

### Next Steps

1. **Review & Approval**: Get stakeholder buy-in on architecture
2. **Prototype**: Build AWS provider proof-of-concept
3. **Phase 1 Implementation**: Start foundation work
4. **Iterate**: Refine based on prototype learnings
5. **Execute**: Follow migration strategy phases

---

## Appendices

### Appendix A: Example Platform Configurations

See examples throughout document for:
- AWS complete cluster setup
- Azure cluster setup
- Multi-nodepool configuration
- Spot instances
- Different instance types

### Appendix B: Security Controls Reference

See [Security Considerations](#security-considerations) section for:
- Validation rules
- Allowed/disallowed operations
- Image registry whitelisting
- Resource limits
- Volume mount restrictions

### Appendix C: API Reference

For complete API definitions, see:
- `providers/aws/api/v1beta1/` for AWS types
- `providers/azure/api/v1beta1/` for Azure types

### Appendix D: Glossary

- **CRD**: Custom Resource Definition
- **CAPI**: Cluster API
- **CPO**: Control Plane Operator
- **HCP**: Hosted Control Plane
- **KAS**: Kube API Server
- **KMS**: Key Management Service
- **Provider**: Platform-specific implementation (AWS, Azure, etc.)
