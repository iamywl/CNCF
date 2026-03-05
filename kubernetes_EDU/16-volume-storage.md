# 16. 볼륨 스토리지 심화

## 목차
1. [개요](#1-개요)
2. [Volume 타입 분류](#2-volume-타입-분류)
3. [VolumeSource 구조체 상세](#3-volumesource-구조체-상세)
4. [PersistentVolume와 PersistentVolumeClaim](#4-persistentvolume와-persistentvolumeclaim)
5. [PV 라이프사이클](#5-pv-라이프사이클)
6. [StorageClass와 동적 프로비저닝](#6-storageclass와-동적-프로비저닝)
7. [CSI (Container Storage Interface)](#7-csi-container-storage-interface)
8. [Volume Plugin Framework](#8-volume-plugin-framework)
9. [Kubelet VolumeManager](#9-kubelet-volumemanager)
10. [Attach/Detach Controller](#10-attachdetach-controller)
11. [PersistentVolume Controller](#11-persistentvolume-controller)
12. [Volume Expansion](#12-volume-expansion)
13. [설계 원칙: Why](#13-설계-원칙-why)

---

## 1. 개요

Kubernetes의 스토리지 시스템은 컨테이너의 임시(ephemeral) 특성을 넘어 **영속적 데이터 관리**를 가능하게 하는 핵심 서브시스템이다. Pod이 재시작되거나 다른 노드로 이동해도 데이터가 유지되어야 하는 데이터베이스, 파일 시스템 등의 워크로드를 지원한다.

Kubernetes 스토리지의 핵심 설계 철학은 다음 세 가지로 요약된다:

1. **스토리지 추상화**: 애플리케이션은 스토리지의 구체적 구현(AWS EBS, GCE PD, NFS 등)을 알 필요 없이, 표준화된 인터페이스(PVC)로 스토리지를 요청한다.
2. **플러그인 기반 확장**: Volume Plugin Framework를 통해 새로운 스토리지 백엔드를 손쉽게 추가할 수 있다.
3. **라이프사이클 분리**: 스토리지의 프로비저닝, 바인딩, 사용, 회수를 명확히 분리하여 관리한다.

### 핵심 소스 파일

| 파일 | 역할 |
|------|------|
| `staging/src/k8s.io/api/core/v1/types.go` | VolumeSource, PV, PVC 타입 정의 |
| `pkg/apis/storage/types.go` | StorageClass, VolumeAttachment 내부 타입 |
| `pkg/volume/plugins.go` | VolumePlugin 인터페이스 계층 |
| `pkg/volume/volume.go` | Volume, Mounter, Unmounter 인터페이스 |
| `pkg/volume/csi/csi_plugin.go` | CSI 플러그인 구현 |
| `pkg/kubelet/volumemanager/volume_manager.go` | kubelet 볼륨 관리자 |
| `pkg/controller/volume/attachdetach/attach_detach_controller.go` | AD 컨트롤러 |
| `pkg/controller/volume/persistentvolume/pv_controller.go` | PV/PVC 바인딩 컨트롤러 |

---

## 2. Volume 타입 분류

Kubernetes의 Volume은 수명(lifetime)과 제공 방식에 따라 크게 네 가지로 분류된다.

### 분류 체계

```
+------------------------------------------------------------------+
|                    Kubernetes Volume Types                        |
+------------------------------------------------------------------+
|                                                                  |
|  [임시 볼륨]          [영속 볼륨]          [프로젝션 볼륨]        |
|  - EmptyDir          - PersistentVolume   - Secret               |
|  - Ephemeral         - CSI Persistent     - ConfigMap            |
|  - Image                                  - DownwardAPI          |
|                                           - Projected            |
|                                           - ServiceAccountToken  |
|  [호스트 볼륨]        [네트워크 볼륨]                             |
|  - HostPath          - NFS                                       |
|                      - iSCSI                                     |
|                      - FC (Fibre Channel)                        |
+------------------------------------------------------------------+
```

### 수명별 분류 테이블

| 분류 | 볼륨 타입 | 수명 | 사용 시나리오 |
|------|----------|------|-------------|
| Pod 수명 | EmptyDir | Pod과 동일 | 컨테이너 간 임시 공유, 캐시 |
| Pod 수명 | Ephemeral | Pod과 동일 | 동적 프로비저닝 임시 스토리지 |
| Pod 수명 | Image | Pod과 동일 | OCI 이미지/아티팩트 마운트 |
| 독립 수명 | PersistentVolumeClaim | PV에 종속 | 데이터베이스, 파일 시스템 |
| 노드 수명 | HostPath | 노드 존재 시 | 시스템 에이전트, DaemonSet |
| 프로젝션 | Secret/ConfigMap | 원본 리소스 | 설정, 인증서 주입 |
| 프로젝션 | DownwardAPI | Pod 메타데이터 | Pod/Container 정보 주입 |
| 네트워크 | NFS/iSCSI/FC | 서버 기준 | 공유 파일 시스템, SAN |

---

## 3. VolumeSource 구조체 상세

`VolumeSource`는 Pod에서 사용할 수 있는 모든 볼륨 유형을 정의하는 Union 타입이다. 정확히 하나의 필드만 설정해야 한다.

**소스 위치**: `staging/src/k8s.io/api/core/v1/types.go` (라인 49-222)

```go
// staging/src/k8s.io/api/core/v1/types.go:49
type VolumeSource struct {
    HostPath              *HostPathVolumeSource              // 호스트 경로 직접 노출
    EmptyDir              *EmptyDirVolumeSource              // Pod 수명 임시 디렉토리
    GCEPersistentDisk     *GCEPersistentDiskVolumeSource     // [Deprecated → CSI]
    AWSElasticBlockStore  *AWSElasticBlockStoreVolumeSource   // [Deprecated → CSI]
    Secret                *SecretVolumeSource                // Secret 데이터 마운트
    NFS                   *NFSVolumeSource                   // NFS 마운트
    ISCSI                 *ISCSIVolumeSource                 // iSCSI 디스크
    PersistentVolumeClaim *PersistentVolumeClaimVolumeSource // PVC 참조
    FlexVolume            *FlexVolumeSource                  // [Deprecated → CSI]
    Cinder                *CinderVolumeSource                // [Deprecated → CSI]
    DownwardAPI           *DownwardAPIVolumeSource           // Pod 메타데이터
    FC                    *FCVolumeSource                    // Fibre Channel
    AzureFile             *AzureFileVolumeSource             // [Deprecated → CSI]
    ConfigMap             *ConfigMapVolumeSource             // ConfigMap 마운트
    VsphereVolume         *VsphereVirtualDiskVolumeSource    // [Deprecated → CSI]
    AzureDisk             *AzureDiskVolumeSource             // [Deprecated → CSI]
    Projected             *ProjectedVolumeSource             // 통합 프로젝션
    PortworxVolume        *PortworxVolumeSource              // [Deprecated → CSI]
    CSI                   *CSIVolumeSource                   // CSI 임시 볼륨
    Ephemeral             *EphemeralVolumeSource             // 동적 임시 볼륨
    Image                 *ImageVolumeSource                 // OCI 이미지 볼륨
}
```

### CSI 마이그레이션 현황

in-tree 볼륨 플러그인들이 CSI로 대량 마이그레이션되었다. 소스코드의 주석을 보면 명확하게 확인할 수 있다:

```
// staging/src/k8s.io/api/core/v1/types.go
// 라인 66-70:
//   Deprecated: GCEPersistentDisk is deprecated. All operations for the
//   in-tree gcePersistentDisk type are redirected to the
//   pd.csi.storage.gke.io CSI driver.
```

| in-tree 타입 | CSI 드라이버 | 상태 |
|-------------|-------------|------|
| GCEPersistentDisk | `pd.csi.storage.gke.io` | Deprecated |
| AWSElasticBlockStore | `ebs.csi.aws.com` | Deprecated |
| AzureDisk | `disk.csi.azure.com` | Deprecated |
| AzureFile | `file.csi.azure.com` | Deprecated |
| Cinder | `cinder.csi.openstack.org` | Deprecated |
| VsphereVolume | `csi.vsphere.vmware.com` | Deprecated |
| PortworxVolume | `pxd.portworx.com` | Deprecated |

### CSIVolumeSource (임시 CSI 볼륨)

**소스 위치**: `staging/src/k8s.io/api/core/v1/types.go` (라인 2265-2293)

```go
// staging/src/k8s.io/api/core/v1/types.go:2265
type CSIVolumeSource struct {
    Driver               string                // CSI 드라이버 이름
    ReadOnly             *bool                 // 읽기 전용 여부
    FSType               *string               // 파일시스템 타입
    VolumeAttributes     map[string]string     // 드라이버별 속성
    NodePublishSecretRef *LocalObjectReference // 시크릿 참조
}
```

### PersistentVolumeSource

PV를 위한 별도의 VolumeSource도 존재한다. 이는 관리자가 생성하는 PV에 사용된다.

**소스 위치**: `staging/src/k8s.io/api/core/v1/types.go` (라인 238-248)

```go
// staging/src/k8s.io/api/core/v1/types.go:238
// PersistentVolumeSource is similar to VolumeSource but meant for the
// administrator who creates PVs. Exactly one of its members must be set.
type PersistentVolumeSource struct {
    GCEPersistentDisk    *GCEPersistentDiskVolumeSource
    AWSElasticBlockStore *AWSElasticBlockStoreVolumeSource
    // ... (VolumeSource와 유사하지만 관리자 전용)
    CSI                  *CSIPersistentVolumeSource  // CSI 영속 볼륨
}
```

---

## 4. PersistentVolume와 PersistentVolumeClaim

PV/PVC는 Kubernetes의 영속 스토리지 추상화의 핵심이다. **PV는 클러스터 리소스**이고, **PVC는 사용자의 스토리지 요청**이다.

### PersistentVolume (PV)

**소스 위치**: `staging/src/k8s.io/api/core/v1/types.go` (라인 364-383)

```go
// staging/src/k8s.io/api/core/v1/types.go:364
type PersistentVolume struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   PersistentVolumeSpec   `json:"spec,omitempty"`
    Status PersistentVolumeStatus `json:"status,omitempty"`
}
```

### PersistentVolumeSpec

**소스 위치**: `staging/src/k8s.io/api/core/v1/types.go` (라인 386-440)

```go
// staging/src/k8s.io/api/core/v1/types.go:386
type PersistentVolumeSpec struct {
    Capacity                      ResourceList
    PersistentVolumeSource        `json:",inline"`      // 볼륨 백엔드
    AccessModes                   []PersistentVolumeAccessMode
    ClaimRef                      *ObjectReference      // PVC와의 양방향 바인딩
    PersistentVolumeReclaimPolicy PersistentVolumeReclaimPolicy
    StorageClassName              string
    MountOptions                  []string
    VolumeMode                    *PersistentVolumeMode // Filesystem | Block
    NodeAffinity                  *VolumeNodeAffinity
    VolumeAttributesClassName     *string               // VolumeAttributesClass
}
```

핵심 필드를 하나씩 살펴보자:

| 필드 | 타입 | 설명 |
|------|------|------|
| `Capacity` | `ResourceList` | 볼륨 용량 (예: `storage: 10Gi`) |
| `PersistentVolumeSource` | embedded | 실제 백엔드 (CSI, NFS 등) |
| `AccessModes` | `[]PersistentVolumeAccessMode` | RWO, ROX, RWX, RWOP |
| `ClaimRef` | `*ObjectReference` | 바인딩된 PVC 참조 |
| `PersistentVolumeReclaimPolicy` | enum | Retain, Delete, Recycle |
| `StorageClassName` | `string` | 소속 StorageClass |
| `VolumeMode` | `*PersistentVolumeMode` | Filesystem 또는 Block |
| `NodeAffinity` | `*VolumeNodeAffinity` | 노드 제약 조건 |

### PersistentVolumeClaim (PVC)

**소스 위치**: `staging/src/k8s.io/api/core/v1/types.go` (라인 514-531)

```go
// staging/src/k8s.io/api/core/v1/types.go:514
type PersistentVolumeClaim struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   PersistentVolumeClaimSpec   `json:"spec,omitempty"`
    Status PersistentVolumeClaimStatus `json:"status,omitempty"`
}
```

### PersistentVolumeClaimSpec

**소스 위치**: `staging/src/k8s.io/api/core/v1/types.go` (라인 548-614)

```go
// staging/src/k8s.io/api/core/v1/types.go:548
type PersistentVolumeClaimSpec struct {
    AccessModes      []PersistentVolumeAccessMode  // 원하는 접근 모드
    Selector         *metav1.LabelSelector         // PV 선택 기준
    Resources        VolumeResourceRequirements    // 최소 리소스 요구
    VolumeName       string                        // 특정 PV 바인딩 (pre-bound)
    StorageClassName *string                       // StorageClass 지정
    VolumeMode       *PersistentVolumeMode         // Filesystem | Block
    DataSource       *TypedLocalObjectReference    // 스냅샷/PVC 복제
    DataSourceRef    *TypedObjectReference         // 크로스 네임스페이스 데이터소스
    VolumeAttributesClassName *string              // VolumeAttributesClass
}
```

### PV와 PVC의 양방향 바인딩

PV와 PVC 사이의 바인딩은 **양방향 포인터**로 구현된다:

```
+--------------------+              +--------------------+
|  PersistentVolume  |              | PersistentVolume   |
|                    |  bind        |   Claim            |
|  spec.claimRef ----+----------->>-+                    |
|    .name           |              |  spec.volumeName --+->> pv.name
|    .namespace      |              |                    |
|    .uid            |              |                    |
+--------------------+              +--------------------+
     (cluster-scoped)                (namespace-scoped)
```

`pv_controller.go` 라인 98-105의 설명에 따르면:

> The fundamental key to this design is the bi-directional "pointer" between
> PersistentVolumes (PVs) and PersistentVolumeClaims (PVCs), which is
> represented here as pvc.Spec.VolumeName and pv.Spec.ClaimRef. The bi-
> directionality is complicated to manage in a transactionless system, but
> without it we can't ensure sane behavior in the face of different forms of
> trouble.

### AccessMode 유형

| 모드 | 약어 | 설명 |
|------|------|------|
| ReadWriteOnce | RWO | 단일 노드에서 읽기/쓰기 |
| ReadOnlyMany | ROX | 다수 노드에서 읽기 전용 |
| ReadWriteMany | RWX | 다수 노드에서 읽기/쓰기 |
| ReadWriteOncePod | RWOP | 단일 Pod에서만 읽기/쓰기 |

---

## 5. PV 라이프사이클

### 상태(Phase) 정의

**소스 위치**: `staging/src/k8s.io/api/core/v1/types.go` (라인 868-884)

```go
// staging/src/k8s.io/api/core/v1/types.go:868
type PersistentVolumePhase string

const (
    VolumePending   PersistentVolumePhase = "Pending"    // 아직 사용 불가
    VolumeAvailable PersistentVolumePhase = "Available"  // 바인딩 대기
    VolumeBound     PersistentVolumePhase = "Bound"      // PVC에 바인딩됨
    VolumeReleased  PersistentVolumePhase = "Released"   // PVC 삭제, 회수 대기
    VolumeFailed    PersistentVolumePhase = "Failed"     // 회수 실패
)
```

### 라이프사이클 다이어그램

```
                        프로비저닝
                            |
                            v
+-----------------------------------------------------------+
|                                                           |
|   [Pending] -----> [Available] -----> [Bound]             |
|                        ^                  |               |
|                        |                  | PVC 삭제      |
|                        |                  v               |
|                   Recycle/재사용     [Released]            |
|                        ^                  |               |
|                        |                  |               |
|                        +------ Recycle ---+               |
|                                           |               |
|                                      Delete/Retain        |
|                                           |               |
|                                           v               |
|                                      [Failed]             |
|                                    (회수 실패 시)          |
+-----------------------------------------------------------+
```

### 회수 정책 (Reclaim Policy)

**소스 위치**: `staging/src/k8s.io/api/core/v1/types.go` (라인 448-462)

```go
// staging/src/k8s.io/api/core/v1/types.go:452
const (
    PersistentVolumeReclaimRecycle PersistentVolumeReclaimPolicy = "Recycle"
    PersistentVolumeReclaimDelete  PersistentVolumeReclaimPolicy = "Delete"
    PersistentVolumeReclaimRetain  PersistentVolumeReclaimPolicy = "Retain"
)
```

| 정책 | 동작 | 기본값 |
|------|------|-------|
| **Retain** | PV를 Released 상태로 유지, 수동 회수 | 수동 생성 PV |
| **Delete** | PV와 백엔드 스토리지 모두 삭제 | 동적 프로비저닝 PV |
| **Recycle** | `rm -rf /volume/*` 후 재사용 가능 | Deprecated |

### VolumeMode

**소스 위치**: `staging/src/k8s.io/api/core/v1/types.go` (라인 464-473)

```go
// staging/src/k8s.io/api/core/v1/types.go:468
const (
    PersistentVolumeBlock      PersistentVolumeMode = "Block"      // 원시 블록 디바이스
    PersistentVolumeFilesystem PersistentVolumeMode = "Filesystem" // 포맷된 파일시스템
)
```

### PV 상태 추적

**소스 위치**: `staging/src/k8s.io/api/core/v1/types.go` (라인 475-492)

```go
// staging/src/k8s.io/api/core/v1/types.go:476
type PersistentVolumeStatus struct {
    Phase                    PersistentVolumePhase // 현재 상태
    Message                  string                // 상태 설명
    Reason                   string                // 실패 사유
    LastPhaseTransitionTime  *metav1.Time          // 마지막 상태 전환 시각
}
```

---

## 6. StorageClass와 동적 프로비저닝

### StorageClass 구조체

**소스 위치**: `pkg/apis/storage/types.go` (라인 33-80)

```go
// pkg/apis/storage/types.go:33
type StorageClass struct {
    metav1.TypeMeta
    metav1.ObjectMeta

    Provisioner          string                            // CSI 드라이버 이름
    Parameters           map[string]string                 // 프로비저너 파라미터
    ReclaimPolicy        *api.PersistentVolumeReclaimPolicy // 회수 정책
    MountOptions         []string                          // 마운트 옵션
    AllowVolumeExpansion *bool                             // 볼륨 확장 허용
    VolumeBindingMode    *VolumeBindingMode                // 바인딩 시점
    AllowedTopologies    []api.TopologySelectorTerm        // 토폴로지 제약
}
```

### 동적 프로비저닝 흐름

```
사용자                 API Server           PV Controller         CSI Driver
  |                       |                      |                    |
  |-- PVC 생성 ---------->|                      |                    |
  |   (storageClassName)  |                      |                    |
  |                       |-- PVC 이벤트 ------->|                    |
  |                       |                      |                    |
  |                       |                      |-- StorageClass     |
  |                       |                      |   조회              |
  |                       |                      |                    |
  |                       |                      |-- Provision ------>|
  |                       |                      |   (CreateVolume)   |
  |                       |                      |                    |
  |                       |                      |<-- Volume 생성 완료 |
  |                       |                      |                    |
  |                       |<-- PV 생성 ----------|                    |
  |                       |                      |                    |
  |                       |<-- PV-PVC 바인딩 ----|                    |
  |                       |                      |                    |
  |<-- PVC Bound 상태 ----|                      |                    |
```

### VolumeBindingMode

| 모드 | 설명 | 사용 시나리오 |
|------|------|-------------|
| `Immediate` | PVC 생성 즉시 프로비저닝/바인딩 | 범용 |
| `WaitForFirstConsumer` | Pod 스케줄링 후 프로비저닝 | 토폴로지 인식 |

`WaitForFirstConsumer`가 중요한 이유:
- Pod이 특정 노드에 스케줄링된 후 해당 노드의 가용 영역(AZ)에서 볼륨을 생성한다.
- 노드와 볼륨의 토폴로지 불일치를 방지한다.
- 예: `us-east-1a`에 있는 노드에 `us-east-1b`의 EBS를 붙이는 실수를 방지한다.

### VolumeAttachment

**소스 위치**: `pkg/apis/storage/types.go` (라인 98-112)

```go
// pkg/apis/storage/types.go:102
type VolumeAttachment struct {
    metav1.TypeMeta
    metav1.ObjectMeta
    Spec   VolumeAttachmentSpec    // 의도하는 attach/detach 동작
    Status VolumeAttachmentStatus  // 현재 attach 상태
}
```

VolumeAttachment은 **볼륨이 특정 노드에 붙어야 한다**는 의도를 표현하는 API 리소스이다.

---

## 7. CSI (Container Storage Interface)

### CSI란?

CSI는 컨테이너 오케스트레이터(Kubernetes)와 스토리지 시스템 사이의 **표준화된 인터페이스**이다. CSI 이전에는 각 스토리지 벤더가 Kubernetes 소스코드를 직접 수정하여 in-tree 플러그인을 추가해야 했다.

### CSI 아키텍처

```
+---Kubernetes Node----------------------------------------------+
|                                                                 |
|  kubelet                                                        |
|    |                                                            |
|    +-- VolumeManager                                            |
|    |     |                                                      |
|    |     +-- CSI Plugin (in-tree)                               |
|    |           |                                                |
|    |           +-- gRPC -----> CSI Driver (사이드카)             |
|    |                           |                                |
|    +-- Plugin Watcher          +-- Node Service                 |
|          |                     |     - NodeStageVolume          |
|          |                     |     - NodePublishVolume        |
|          +-- Registration      |     - NodeGetCapabilities     |
|               Socket           |                                |
|                                +-- Identity Service             |
|                                      - GetPluginInfo           |
|                                      - GetPluginCapabilities   |
+-----------------------------------------------------------------+

+---Control Plane-------------------------------------------------+
|                                                                  |
|  kube-controller-manager                                         |
|    |                                                             |
|    +-- Attach/Detach Controller                                  |
|          |                                                       |
|          +-- CSI Attacher (사이드카) -----> CSI Driver            |
|                                             |                    |
|                                             +-- Controller Svc   |
|                                                  - CreateVolume  |
|                                                  - DeleteVolume  |
|                                                  - ControllerPublish |
|                                                  - ControllerUnpublish|
+------------------------------------------------------------------+
```

### CSI Plugin 구현

**소스 위치**: `pkg/volume/csi/csi_plugin.go` (라인 52-101)

```go
// pkg/volume/csi/csi_plugin.go:53
const (
    CSIPluginName   = "kubernetes.io/csi"
    csiTimeout      = 2 * time.Minute
    volNameSep      = "^"
    volDataFileName = "vol_data.json"
    fsTypeBlockName = "block"
    CsiResyncPeriod = time.Minute
)

// pkg/volume/csi/csi_plugin.go:66
type csiPlugin struct {
    host                      volume.VolumeHost
    csiDriverLister           storagelisters.CSIDriverLister
    csiDriverInformer         cache.SharedIndexInformer
    serviceAccountTokenGetter func(namespace, name string, tr *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error)
    volumeAttachmentLister    storagelisters.VolumeAttachmentLister
}
```

### CSI 드라이버 등록 흐름

**소스 위치**: `pkg/volume/csi/csi_plugin.go` (라인 74-119)

```go
// pkg/volume/csi/csi_plugin.go:75
func ProbeVolumePlugins() []volume.VolumePlugin {
    p := &csiPlugin{host: nil}
    return []volume.VolumePlugin{p}
}

// pkg/volume/csi/csi_plugin.go:93
var csiDrivers = &DriversStore{}

// pkg/volume/csi/csi_plugin.go:101
var PluginHandler = &RegistrationHandler{}
```

CSI 드라이버 등록 과정:
1. kubelet의 PluginWatcher가 등록 소켓(`/var/lib/kubelet/plugins_registry/`)을 감시
2. 새 CSI 드라이버 소켓 감지 시 `ValidatePlugin()` 호출
3. 검증 통과 시 `RegisterPlugin()` 으로 등록
4. `csiDrivers` (DriversStore)에 드라이버 정보 저장

### CSI 핵심 파일 구조

```
pkg/volume/csi/
├── csi_plugin.go          # 플러그인 인터페이스 구현, 드라이버 등록
├── csi_attacher.go        # ControllerPublish/Unpublish (attach/detach)
├── csi_mounter.go         # NodeStage/Publish (mount/unmount)
├── csi_block.go           # 블록 볼륨 지원
├── csi_client.go          # gRPC 클라이언트
├── csi_drivers_store.go   # 등록된 드라이버 관리
├── csi_metrics.go         # 메트릭 수집
├── csi_node_updater.go    # CSINode 리소스 업데이트
├── csi_util.go            # 유틸리티
└── expander.go            # 볼륨 확장
```

---

## 8. Volume Plugin Framework

### 인터페이스 계층 구조

Kubernetes의 Volume Plugin Framework는 여러 레벨의 인터페이스로 구성된다. 각 플러그인은 자신이 지원하는 기능에 해당하는 인터페이스를 구현한다.

**소스 위치**: `pkg/volume/plugins.go` (라인 128-283)

```
                    VolumePlugin (기본)
                         |
            +------------+-------------+
            |            |             |
    PersistentVolume  Attachable    Expandable
      Plugin         VolumePlugin  VolumePlugin
            |            |             |
            |     DeviceMountable      |
            |      VolumePlugin   NodeExpandable
            |                     VolumePlugin
            |
    +-------+-------+
    |       |       |
 Recyclable Deletable Provisionable
  Plugin     Plugin    Plugin

            BlockVolumePlugin
```

### VolumePlugin (기본 인터페이스)

**소스 위치**: `pkg/volume/plugins.go` (라인 128-181)

```go
// pkg/volume/plugins.go:128
type VolumePlugin interface {
    Init(host VolumeHost) error
    GetPluginName() string
    GetVolumeName(spec *Spec) (string, error)
    CanSupport(spec *Spec) bool
    RequiresRemount(spec *Spec) bool
    NewMounter(spec *Spec, podRef *v1.Pod) (Mounter, error)
    NewUnmounter(name string, podUID types.UID) (Unmounter, error)
    ConstructVolumeSpec(volumeName, volumePath string) (ReconstructedVolume, error)
    SupportsMountOption() bool
    SupportsSELinuxContextMount(spec *Spec) (bool, error)
}
```

### 확장 인터페이스

```go
// pkg/volume/plugins.go:185
type PersistentVolumePlugin interface {
    VolumePlugin
    GetAccessModes() []v1.PersistentVolumeAccessMode
}

// pkg/volume/plugins.go:228
type AttachableVolumePlugin interface {
    DeviceMountableVolumePlugin
    NewAttacher() (Attacher, error)
    NewDetacher() (Detacher, error)
    CanAttach(spec *Spec) (bool, error)
    VerifyExhaustedResource(spec *Spec) bool
}

// pkg/volume/plugins.go:250
type ExpandableVolumePlugin interface {
    VolumePlugin
    ExpandVolumeDevice(spec *Spec, newSize resource.Quantity, oldSize resource.Quantity) (resource.Quantity, error)
    RequiresFSResize() bool
}

// pkg/volume/plugins.go:258
type NodeExpandableVolumePlugin interface {
    VolumePlugin
    RequiresFSResize() bool
    NodeExpand(resizeOptions NodeResizeOptions) (bool, error)
}

// pkg/volume/plugins.go:266
type BlockVolumePlugin interface {
    VolumePlugin
    NewBlockVolumeMapper(spec *Spec, podRef *v1.Pod) (BlockVolumeMapper, error)
    NewBlockVolumeUnmapper(name string, podUID types.UID) (BlockVolumeUnmapper, error)
    ConstructBlockVolumeSpec(podUID types.UID, volumeName, volumePath string) (*Spec, error)
}
```

### Volume 인터페이스 (인스턴스 레벨)

**소스 위치**: `pkg/volume/volume.go` (라인 32-41)

```go
// pkg/volume/volume.go:32
type Volume interface {
    GetPath() string     // 마운트 경로
    MetricsProvider      // 메트릭 (사용량, 용량)
}
```

### Mounter/Unmounter

```go
// volume.go에서의 Mounter 관련 구조
type MounterArgs struct {
    FsUser              *int64
    FsGroup             *int64
    FSGroupChangePolicy *v1.PodFSGroupChangePolicy
    DesiredSize         *resource.Quantity
    SELinuxLabel        string
    Recorder            record.EventRecorder
}
```

### VolumeHost 인터페이스

**소스 위치**: `pkg/volume/plugins.go` (라인 340-350)

VolumeHost는 플러그인이 kubelet이나 컨트롤러와 상호작용하기 위한 인터페이스이다.

```go
// pkg/volume/plugins.go:341
type VolumeHost interface {
    GetPluginDir(pluginName string) string
    GetVolumeDevicePluginDir(pluginName string) string
    GetPodsDir() string
    GetPodVolumeDir(podUID types.UID, pluginName string, volumeName string) string
    // ... 추가 메서드
}
```

특화된 VolumeHost 인터페이스:

```go
// pkg/volume/plugins.go:294
type KubeletVolumeHost interface {
    SetKubeletError(err error)
    GetInformerFactory() informers.SharedInformerFactory
    CSIDriverLister() storagelistersv1.CSIDriverLister
    CSIDriversSynced() cache.InformerSynced
    WaitForCacheSync() error
    GetHostUtil() hostutil.HostUtils
    GetTrustAnchorsByName(name string, allowMissing bool) ([]byte, error)
    GetTrustAnchorsBySigner(signerName string, ...) ([]byte, error)
}

// pkg/volume/plugins.go:329
type AttachDetachVolumeHost interface {
    CSIDriverVolumeHost
    CSINodeLister() storagelistersv1.CSINodeLister
    VolumeAttachmentLister() storagelistersv1.VolumeAttachmentLister
    IsAttachDetachController() bool
}
```

### VolumeOptions

**소스 위치**: `pkg/volume/plugins.go` (라인 74-96)

```go
// pkg/volume/plugins.go:75
type VolumeOptions struct {
    PersistentVolumeReclaimPolicy v1.PersistentVolumeReclaimPolicy
    MountOptions                  []string
    PVName                        string              // 고유 PV 이름
    PVC                           *v1.PersistentVolumeClaim
    Parameters                    map[string]string    // StorageClass 파라미터
}
```

---

## 9. Kubelet VolumeManager

### 개요

VolumeManager는 kubelet 내부에서 동작하며, Pod이 필요로 하는 볼륨이 올바르게 attach/mount 되어 있는지를 보장하는 비동기 루프 시스템이다.

**소스 위치**: `pkg/kubelet/volumemanager/volume_manager.go` (라인 94-160)

```go
// pkg/kubelet/volumemanager/volume_manager.go:94
type VolumeManager interface {
    Run(ctx context.Context, sourcesReady config.SourcesReady)
    WaitForAttachAndMount(ctx context.Context, pod *v1.Pod) error
    WaitForUnmount(ctx context.Context, pod *v1.Pod) error
    WaitForAllPodsUnmount(ctx context.Context, pods []*v1.Pod) error
    GetMountedVolumesForPod(podName types.UniquePodName) container.VolumeMap
    HasPossiblyMountedVolumesForPod(podName types.UniquePodName) bool
    GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64
    GetVolumesInUse() []v1.UniqueVolumeName
    ReconcilerStatesHasBeenSynced() bool
    VolumeIsAttached(volumeName v1.UniqueVolumeName) bool
    MarkVolumesAsReportedInUse(volumesReportedAsInUse []v1.UniqueVolumeName)
}
```

### 내부 구조

**소스 위치**: `pkg/kubelet/volumemanager/volume_manager.go` (라인 242-284)

```go
// pkg/kubelet/volumemanager/volume_manager.go:243
type volumeManager struct {
    kubeClient                  clientset.Interface
    volumePluginMgr             *volume.VolumePluginMgr
    desiredStateOfWorld         cache.DesiredStateOfWorld      // 원하는 상태
    actualStateOfWorld          cache.ActualStateOfWorld       // 실제 상태
    operationExecutor           operationexecutor.OperationExecutor
    reconciler                  reconciler.Reconciler
    desiredStateOfWorldPopulator populator.DesiredStateOfWorldPopulator
    csiMigratedPluginManager    csimigration.PluginManager
    intreeToCSITranslator       csimigration.InTreeToCSITranslator
}
```

### 핵심 컴포넌트

```
+---VolumeManager-------------------------------------------------+
|                                                                  |
|  DesiredStateOfWorld          ActualStateOfWorld                  |
|  Populator                                                       |
|       |                            ^                             |
|       | 100ms 주기로               | attach/mount/unmount/       |
|       | Pod 정보 수집              | detach 결과 반영             |
|       v                            |                             |
|  DesiredState ----Reconciler----> ActualState                    |
|  OfWorld           100ms 주기     OfWorld                        |
|                    비교 + 조정                                    |
|                         |                                        |
|                         v                                        |
|               OperationExecutor                                  |
|               (비동기 작업 실행)                                   |
+------------------------------------------------------------------+
```

### 초기화 및 실행

**소스 위치**: `pkg/kubelet/volumemanager/volume_manager.go` (라인 183-240)

```go
// pkg/kubelet/volumemanager/volume_manager.go:183
func NewVolumeManager(
    controllerAttachDetachEnabled bool,
    nodeName k8stypes.NodeName,
    podManager PodManager,
    podStateProvider PodStateProvider,
    kubeClient clientset.Interface,
    volumePluginMgr *volume.VolumePluginMgr,
    mounter mount.Interface,
    hostutil hostutil.HostUtils,
    kubeletPodsDir string,
    recorder record.EventRecorder,
    blockVolumePathHandler volumepathhandler.BlockVolumePathHandler,
) VolumeManager {
    // ...
    vm := &volumeManager{
        kubeClient:          kubeClient,
        volumePluginMgr:     volumePluginMgr,
        desiredStateOfWorld: cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator),
        actualStateOfWorld:  cache.NewActualStateOfWorld(nodeName, volumePluginMgr),
        operationExecutor:   operationexecutor.NewOperationExecutor(...),
    }
    // ...
}
```

### Run 메서드

**소스 위치**: `pkg/kubelet/volumemanager/volume_manager.go` (라인 298-317)

```go
// pkg/kubelet/volumemanager/volume_manager.go:298
func (vm *volumeManager) Run(ctx context.Context, sourcesReady config.SourcesReady) {
    if vm.kubeClient != nil {
        go vm.volumePluginMgr.Run(ctx.Done())          // CSIDriver 인포머
    }
    go vm.desiredStateOfWorldPopulator.Run(ctx, sourcesReady) // DSW 채우기
    go vm.reconciler.Run(ctx, ctx.Done())                      // 조정 루프
    metrics.Register(vm.actualStateOfWorld, vm.desiredStateOfWorld, vm.volumePluginMgr)
    <-ctx.Done()
}
```

### 타이밍 상수

**소스 위치**: `pkg/kubelet/volumemanager/volume_manager.go` (라인 57-92)

| 상수 | 값 | 설명 |
|------|-----|------|
| `reconcilerLoopSleepPeriod` | 100ms | Reconciler 루프 주기 |
| `desiredStateOfWorldPopulatorLoopSleepPeriod` | 100ms | DSW Populator 주기 |
| `podAttachAndMountTimeout` | 2m 3s | Pod 볼륨 마운트 대기 한도 |
| `podAttachAndMountRetryInterval` | 300ms | 마운트 재시도 간격 |
| `waitForAttachTimeout` | 10m | attach 대기 최대 시간 |

---

## 10. Attach/Detach Controller

### 개요

Attach/Detach(AD) 컨트롤러는 kube-controller-manager에서 실행되며, 볼륨을 적절한 노드에 attach/detach 하는 역할을 담당한다.

**소스 위치**: `pkg/controller/volume/attachdetach/attach_detach_controller.go`

### 구조

```go
// pkg/controller/volume/attachdetach/attach_detach_controller.go:96
type AttachDetachController interface {
    Run(ctx context.Context)
    GetDesiredStateOfWorld() cache.DesiredStateOfWorld
}
```

### 초기화 매개변수

**소스 위치**: `pkg/controller/volume/attachdetach/attach_detach_controller.go` (라인 103-118)

```go
func NewAttachDetachController(
    ctx context.Context,
    kubeClient clientset.Interface,
    podInformer coreinformers.PodInformer,
    nodeInformer coreinformers.NodeInformer,
    pvcInformer coreinformers.PersistentVolumeClaimInformer,
    pvInformer coreinformers.PersistentVolumeInformer,
    csiNodeInformer storageinformersv1.CSINodeInformer,
    csiDriverInformer storageinformersv1.CSIDriverInformer,
    volumeAttachmentInformer storageinformersv1.VolumeAttachmentInformer,
    plugins []volume.VolumePlugin,
    prober volume.DynamicPluginProber,
    disableReconciliationSync bool,
    reconcilerSyncDuration time.Duration,
    disableForceDetachOnTimeout bool,
    timerConfig TimerConfig,
) (AttachDetachController, error)
```

### 타이머 설정

**소스 위치**: `pkg/controller/volume/attachdetach/attach_detach_controller.go` (라인 63-94)

```go
// pkg/controller/volume/attachdetach/attach_detach_controller.go:66
type TimerConfig struct {
    ReconcilerLoopPeriod                              time.Duration
    ReconcilerMaxWaitForUnmountDuration               time.Duration
    DesiredStateOfWorldPopulatorLoopSleepPeriod        time.Duration
    DesiredStateOfWorldPopulatorListPodsRetryDuration  time.Duration
}

var DefaultTimerConfig = TimerConfig{
    ReconcilerLoopPeriod:                              100 * time.Millisecond,
    ReconcilerMaxWaitForUnmountDuration:               6 * time.Minute,
    DesiredStateOfWorldPopulatorLoopSleepPeriod:       1 * time.Minute,
    DesiredStateOfWorldPopulatorListPodsRetryDuration: 3 * time.Minute,
}
```

### AD 컨트롤러 디렉토리 구조

```
pkg/controller/volume/attachdetach/
├── attach_detach_controller.go     # 메인 컨트롤러
├── cache/
│   ├── actual_state_of_world.go    # 실제 attach 상태
│   └── desired_state_of_world.go   # 원하는 attach 상태
├── populator/
│   └── desired_state_of_world_populator.go  # DSW 채우기
├── reconciler/
│   └── reconciler.go               # DSW vs ASW 조정
├── statusupdater/
│   └── node_status_updater.go      # Node 상태 업데이트
├── metrics/
│   └── metrics.go                  # 메트릭
└── util/
    └── util.go                     # 유틸리티
```

### Attach/Detach 흐름

```
                     API Server
                         |
          +------+-------+-------+------+
          |      |               |      |
       Pod     Node            PVC     PV
      Informer Informer      Informer Informer
          |      |               |      |
          v      v               v      v
     +-----DesiredStateOfWorld Populator-----+
     |                                       |
     |   "이 Pod은 이 노드에 이 볼륨이 필요"  |
     +-------------------+-------------------+
                         |
                         v
     +------------Reconciler-----------------+
     |                                       |
     |   DesiredState != ActualState ?        |
     |   - 필요한 attach  -> attach 실행      |
     |   - 불필요한 attach -> detach 실행      |
     +-------------------+-------------------+
                         |
                         v
     +--------OperationExecutor--------------+
     |                                       |
     |  VolumeAttachment 생성/삭제           |
     |  -> CSI ControllerPublish/Unpublish   |
     +---------------------------------------+
```

---

## 11. PersistentVolume Controller

### "Space Shuttle" 스타일 코드

PV Controller의 코드는 특별한 "Space Shuttle 스타일"로 작성되어 있다. 이는 모든 조건과 분기를 명시적으로 처리하는 방식이다.

**소스 위치**: `pkg/controller/volume/persistentvolume/pv_controller.go` (라인 62-94)

```go
// ==================================================================
// PLEASE DO NOT ATTEMPT TO SIMPLIFY THIS CODE.
// KEEP THE SPACE SHUTTLE FLYING.
// ==================================================================
//
// This controller is intentionally written in a very verbose style.
// You will notice:
//
// 1. Every 'if' statement has a matching 'else'
// 2. Things that may seem obvious are commented explicitly
//
// We call this style 'space shuttle style'. Space shuttle style is
// meant to ensure that every branch and condition is considered and
// accounted for - the same way code is written at NASA for
// applications like the space shuttle.
```

### PersistentVolumeController 구조체

**소스 위치**: `pkg/controller/volume/persistentvolume/pv_controller.go` (라인 144-220)

```go
// pkg/controller/volume/persistentvolume/pv_controller.go:144
type PersistentVolumeController struct {
    volumeLister       corelisters.PersistentVolumeLister
    volumeListerSynced cache.InformerSynced
    claimLister        corelisters.PersistentVolumeClaimLister
    claimListerSynced  cache.InformerSynced
    classLister        storagelisters.StorageClassLister
    classListerSynced  cache.InformerSynced
    podLister          corelisters.PodLister
    podListerSynced    cache.InformerSynced

    kubeClient                clientset.Interface
    eventBroadcaster          record.EventBroadcaster
    eventRecorder             record.EventRecorder
    volumePluginMgr           vol.VolumePluginMgr
    enableDynamicProvisioning bool

    // 로컬 캐시: etcd 이벤트 + 컨트롤러 업데이트 모두 반영
    volumes persistentVolumeOrderedIndex
    claims  cache.Store

    // 작업 큐: 정확히 하나의 워커 스레드
    claimQueue  workqueue.TypedRateLimitingInterface[string]
    volumeQueue workqueue.TypedRateLimitingInterface[string]

    // 동시 실행 맵
    runningOperations goroutinemap.GoRoutineMap

    createProvisionedPVRetryCount int
    createProvisionedPVInterval   time.Duration
}
```

### 바인딩 알고리즘 (syncClaim)

PV Controller의 핵심 로직은 `syncClaim()` 메서드이다. 이 메서드는 PVC의 상태에 따라 적절한 PV를 찾아 바인딩한다.

```
syncClaim(claim)
    |
    +-- claim이 이미 바인딩됨?
    |     |
    |     +-- Yes: syncBoundClaim()
    |     |         - PV가 존재하는지 확인
    |     |         - PV.ClaimRef가 이 claim인지 확인
    |     |
    |     +-- No: 바인딩되지 않은 claim 처리
    |           |
    |           +-- 매칭되는 PV 검색
    |           |     |
    |           |     +-- 찾음: bindVolumeToClaim() -> 양방향 바인딩
    |           |     |
    |           |     +-- 못 찾음: 동적 프로비저닝 시도
    |           |           |
    |           |           +-- StorageClass 존재?
    |           |                 |
    |           |                 +-- Yes: provisionClaim()
    |           |                 |
    |           |                 +-- No: claim을 Pending 상태로 유지
    |           |
    +-- syncVolume(volume)
          |
          +-- volume이 바인딩됨?
                |
                +-- Yes: claim이 아직 존재하는지 확인
                |         - 없으면 Released 상태로 전환
                |         - 있으면 바인딩 유지
                |
                +-- No: Available 상태로 유지
```

### 왜 양방향 바인딩인가?

소스코드의 주석(라인 98-122)에 명확히 설명되어 있다:

1. **레이스 컨디션 방지**: HA 환경에서 두 컨트롤러 인스턴스가 동시에 다른 볼륨을 같은 claim에 바인딩하는 것을 방지
2. **데이터 손실 방지**: 양방향 확인 없이는 한 볼륨이 두 claim에 바인딩될 수 있음
3. **트랜잭션 없는 시스템**: etcd는 트랜잭션을 보장하지 않으므로, 양방향 포인터로 일관성 확보

---

## 12. Volume Expansion

### 볼륨 확장 흐름

볼륨 확장은 두 단계로 이루어진다:

```
1. Control Plane 확장 (ExpandVolumeDevice)
   - StorageClass.AllowVolumeExpansion = true 필요
   - PVC의 spec.resources.requests.storage 변경
   - PV Controller가 CSI ControllerExpandVolume 호출

2. Node 확장 (NodeExpand)
   - 파일시스템 리사이즈 필요 시
   - kubelet VolumeManager가 CSI NodeExpandVolume 호출
```

### 관련 인터페이스

**소스 위치**: `pkg/volume/plugins.go` (라인 248-263)

```go
// Control Plane 확장
type ExpandableVolumePlugin interface {
    VolumePlugin
    ExpandVolumeDevice(spec *Spec, newSize resource.Quantity, oldSize resource.Quantity) (resource.Quantity, error)
    RequiresFSResize() bool
}

// Node 확장
type NodeExpandableVolumePlugin interface {
    VolumePlugin
    RequiresFSResize() bool
    NodeExpand(resizeOptions NodeResizeOptions) (bool, error)
}
```

### NodeResizeOptions

**소스 위치**: `pkg/volume/plugins.go` (라인 98-116)

```go
// pkg/volume/plugins.go:99
type NodeResizeOptions struct {
    VolumeSpec     *Spec
    DevicePath     string           // 실제 디바이스 경로
    DeviceMountPath string          // 디바이스 마운트 경로
    DeviceStagePath string          // 스테이징 경로
    NewSize        resource.Quantity
    OldSize        resource.Quantity
}
```

### Volume Expand Controller

**소스 위치**: `pkg/controller/volume/expand/expand_controller.go`

Expand Controller는 PVC의 리사이즈 요청을 감시하고, CSI 드라이버에게 볼륨 확장을 요청한다.

---

## 13. 설계 원칙: Why

### Why: 스토리지 추상화가 필요한 이유

1. **벤더 독립성**: 애플리케이션 코드가 특정 스토리지 벤더에 종속되지 않는다. PVC 하나로 AWS EBS, GCE PD, Ceph 등을 투명하게 전환할 수 있다.

2. **역할 분리**:
   - 클러스터 관리자: StorageClass와 PV를 관리
   - 개발자: PVC만으로 스토리지를 요청
   - CSI 드라이버 벤더: 표준 인터페이스만 구현

3. **라이프사이클 독립**: Pod의 수명과 데이터의 수명을 분리함으로써, Pod이 죽어도 데이터가 유지된다.

### Why: CSI가 in-tree 플러그인을 대체한 이유

소스코드에서 다수의 in-tree 플러그인이 Deprecated 주석과 함께 CSI 드라이버로 리다이렉트되고 있다:

```go
// staging/src/k8s.io/api/core/v1/types.go:66-70
// Deprecated: GCEPersistentDisk is deprecated. All operations for the
// in-tree gcePersistentDisk type are redirected to the
// pd.csi.storage.gke.io CSI driver.
```

이유:
1. **독립적 릴리스**: CSI 드라이버는 Kubernetes와 별도로 릴리스 가능
2. **컴파일 의존성 제거**: in-tree는 Kubernetes 소스에 직접 포함되어야 함
3. **표준화**: CSI는 CNCF 표준으로 Docker, Mesos 등에서도 동일하게 사용
4. **보안**: out-of-tree 방식으로 권한 최소화 가능

### Why: 양방향 바인딩이 필수적인 이유

PV Controller의 "Space Shuttle" 주석(라인 96-122)이 이를 명확히 설명한다:

```
PV.Spec.ClaimRef (PV → PVC 방향)
  +
PVC.Spec.VolumeName (PVC → PV 방향)
```

트랜잭션 없는 분산 시스템에서:
- 단방향 바인딩만으로는 두 컨트롤러가 동시에 같은 PV를 다른 PVC에 바인딩할 수 있다
- 양방향 확인으로 "이 PV는 정확히 이 PVC의 것"임을 양쪽 모두에서 보장한다
- 레이스 컨디션 시 version conflict로 자동 복구된다

### Why: DesiredState vs ActualState 패턴을 사용하는 이유

VolumeManager와 AD Controller 모두 이 패턴을 사용한다:

```go
// pkg/kubelet/volumemanager/volume_manager.go:252-264
desiredStateOfWorld  cache.DesiredStateOfWorld  // 목표 상태
actualStateOfWorld   cache.ActualStateOfWorld   // 현재 상태
reconciler           reconciler.Reconciler      // 차이 해소
```

이유:
1. **선언적 모델**: "무엇이 되어야 하는지"와 "무엇인지"를 분리하여 수렴 가능
2. **장애 복구**: 중간에 실패해도 reconciler가 다음 루프에서 재시도
3. **단순한 추론**: 원하는 상태만 설정하면 시스템이 알아서 수렴
4. **100ms 주기**: 빠른 감지와 조정으로 볼륨 상태 불일치 최소화

### Why: 워커 스레드가 하나인 이유 (PV Controller)

PV Controller는 `claimQueue`와 `volumeQueue`에 각각 하나의 워커만 사용한다:

```go
// pkg/controller/volume/persistentvolume/pv_controller.go:187-194
// Work queues of claims and volumes to process. Every queue should
// have exactly one worker thread, especially syncClaim() is not
// reentrant. Two syncClaims could bind two different claims to the
// same volume or one claim to two volumes.
```

이유:
- `syncClaim()`이 재진입(reentrant) 불가
- 두 워커가 동시에 실행되면 같은 PV를 두 PVC에 바인딩할 수 있음
- 단일 워커로 직렬 처리하는 것이 전체 처리량 대비 안전성이 훨씬 높음

### Why: 메트릭과 모니터링이 필수인 이유

볼륨은 Pod 시작의 병목이 될 수 있다:

```go
// pkg/kubelet/volumemanager/volume_manager.go:82-87
// waitForAttachTimeout = 10 * time.Minute
// Set to 10 minutes because we've seen attach operations take
// several minutes to complete for some volume plugins in some cases.
```

- attach에 최대 10분이 소요될 수 있다
- 이 시간 동안 다른 디바이스 작업은 영향받지 않는다 (디바이스별 독립)
- Pod 시작 대기 시간 모니터링이 운영에 필수적이다

---

## 요약

Kubernetes 스토리지 시스템은 여러 레이어의 추상화로 구성된다:

```
+---사용자 레이어--------------------------------------------------+
|  PVC (PersistentVolumeClaim) - 스토리지 요청                     |
|  StorageClass - 동적 프로비저닝 정책                              |
+------------------------------------------------------------------+
                              |
+---컨트롤 플레인 레이어---------------------------------------+
|  PV Controller - 바인딩, 프로비저닝, 회수                     |
|  AD Controller - attach/detach 관리                           |
|  Expand Controller - 볼륨 확장                                |
+--------------------------------------------------------------+
                              |
+---노드 레이어----------------------------------------------------+
|  kubelet VolumeManager - mount/unmount, 상태 조정                |
|  Volume Plugin Framework - 플러그인 인터페이스                    |
+------------------------------------------------------------------+
                              |
+---스토리지 레이어------------------------------------------------+
|  CSI Driver - 실제 스토리지 백엔드와 통신                        |
|  in-tree plugins - (대부분 CSI로 마이그레이션)                    |
+------------------------------------------------------------------+
```

각 레이어는 명확한 책임을 가지며, CSI를 통해 스토리지 백엔드를 교체 가능하게 설계되어 있다. "Space Shuttle 스타일"의 PV Controller는 모든 경우의 수를 명시적으로 처리하여, 분산 시스템에서의 데이터 안전성을 보장한다.
