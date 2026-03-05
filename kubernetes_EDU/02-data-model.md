# 02. Kubernetes 데이터 모델

## 개요

Kubernetes의 모든 리소스는 **TypeMeta + ObjectMeta + Spec + Status** 패턴을 따른다.
이 패턴은 선언적 API의 핵심으로, Spec에 원하는 상태를, Status에 현재 상태를 담는다.

## API 객체 기본 구조

### TypeMeta — 리소스 타입 식별

소스: `staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go:42-57`

```go
type TypeMeta struct {
    Kind       string `json:"kind,omitempty"`       // 리소스 종류 (Pod, Service, ...)
    APIVersion string `json:"apiVersion,omitempty"` // API 버전 (v1, apps/v1, ...)
}
```

### ObjectMeta — 리소스 메타데이터

소스: `staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go:111`

```go
type ObjectMeta struct {
    Name              string            // 리소스 이름 (네임스페이스 내 고유)
    GenerateName      string            // 이름 자동 생성 접두사
    Namespace         string            // 네임스페이스
    UID               types.UID         // 시스템 할당 고유 ID
    ResourceVersion   string            // etcd revision (낙관적 동시성 제어)
    Generation        int64             // Spec 변경 시 증가
    CreationTimestamp Time              // 생성 시각
    DeletionTimestamp *Time             // 삭제 요청 시각 (graceful delete)
    Labels            map[string]string // 라벨 (셀렉터로 검색)
    Annotations       map[string]string // 어노테이션 (비검색 메타데이터)
    OwnerReferences   []OwnerReference  // 소유자 참조 (GC용)
    Finalizers        []string          // 삭제 전 정리 작업
    ManagedFields     []ManagedFieldsEntry // Server-Side Apply 필드 관리
}
```

### ListMeta — 목록 메타데이터

소스: `staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go:61-95`

```go
type ListMeta struct {
    ResourceVersion    string // 목록 시점의 리소스 버전
    Continue           string // 페이지네이션 토큰
    RemainingItemCount *int64 // 남은 항목 수
}
```

## 핵심 리소스 타입

### Pod — 컨테이너 실행 단위

소스: `staging/src/k8s.io/api/core/v1/types.go:5463-5482`

```go
type Pod struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   PodSpec    `json:"spec,omitempty"`    // 원하는 상태
    Status PodStatus  `json:"status,omitempty"`  // 현재 상태
}
```

**PodSpec 주요 필드** (line 4118):

| 필드 | 타입 | 설명 |
|------|------|------|
| Containers | []Container | 실행할 컨테이너 목록 |
| InitContainers | []Container | 초기화 컨테이너 |
| Volumes | []Volume | 마운트할 볼륨 |
| NodeName | string | 스케줄된 노드 이름 |
| NodeSelector | map[string]string | 노드 선택 조건 |
| ServiceAccountName | string | 서비스 계정 |
| RestartPolicy | RestartPolicy | 재시작 정책 (Always/OnFailure/Never) |
| SchedulerName | string | 사용할 스케줄러 |
| Tolerations | []Toleration | 노드 Taint 허용 |
| Affinity | *Affinity | 노드/Pod 친화성 |

**PodStatus 주요 필드**:

| 필드 | 타입 | 설명 |
|------|------|------|
| Phase | PodPhase | Pending/Running/Succeeded/Failed/Unknown |
| Conditions | []PodCondition | 상세 조건 (Ready, Scheduled, ...) |
| PodIP | string | Pod IP 주소 |
| HostIP | string | 호스트 노드 IP |
| ContainerStatuses | []ContainerStatus | 컨테이너별 상태 |
| StartTime | *Time | 시작 시각 |

### Service — 서비스 추상화

소스: `staging/src/k8s.io/api/core/v1/types.go:6251-6269`

```go
type Service struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ServiceSpec   `json:"spec,omitempty"`
    Status ServiceStatus `json:"status,omitempty"`
}
```

**ServiceSpec 주요 필드**:

| 필드 | 타입 | 설명 |
|------|------|------|
| Type | ServiceType | ClusterIP/NodePort/LoadBalancer/ExternalName |
| Selector | map[string]string | 대상 Pod 셀렉터 |
| Ports | []ServicePort | 포트 매핑 |
| ClusterIP | string | 클러스터 내부 IP |
| ExternalIPs | []string | 외부 IP |
| SessionAffinity | ServiceAffinity | 세션 친화성 |

### Node — 워커 노드

소스: `staging/src/k8s.io/api/core/v1/types.go:6988-7006`

```go
type Node struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   NodeSpec   `json:"spec,omitempty"`
    Status NodeStatus `json:"status,omitempty"`
}
```

**NodeStatus 주요 필드**:

| 필드 | 타입 | 설명 |
|------|------|------|
| Capacity | ResourceList | 노드 총 리소스 (CPU, Memory, Pods) |
| Allocatable | ResourceList | 할당 가능 리소스 |
| Conditions | []NodeCondition | Ready/MemoryPressure/DiskPressure |
| Addresses | []NodeAddress | InternalIP, Hostname |
| NodeInfo | NodeSystemInfo | OS, 커널, 런타임 버전 |

### Deployment — 선언적 배포

소스: `staging/src/k8s.io/api/apps/v1/types.go:362-376`

```go
type Deployment struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   DeploymentSpec   `json:"spec,omitempty"`
    Status DeploymentStatus `json:"status,omitempty"`
}
```

**DeploymentSpec 주요 필드** (line 378):

| 필드 | 타입 | 설명 |
|------|------|------|
| Replicas | *int32 | 원하는 Pod 수 |
| Selector | *LabelSelector | Pod 셀렉터 |
| Template | PodTemplateSpec | Pod 템플릿 |
| Strategy | DeploymentStrategy | RollingUpdate/Recreate |
| RevisionHistoryLimit | *int32 | 유지할 ReplicaSet 히스토리 수 |

## 리소스 계층 구조

```
Deployment (apps/v1)
  └─ ReplicaSet (apps/v1)          ← OwnerReference → Deployment
       └─ Pod (core/v1)            ← OwnerReference → ReplicaSet

StatefulSet (apps/v1)
  └─ Pod (core/v1)                 ← OwnerReference → StatefulSet
       └─ PersistentVolumeClaim    ← volumeClaimTemplates로 생성

Job (batch/v1)
  └─ Pod (core/v1)                 ← OwnerReference → Job

CronJob (batch/v1)
  └─ Job (batch/v1)                ← OwnerReference → CronJob
       └─ Pod (core/v1)
```

## OwnerReference — 소유자 참조

소스: `staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go`

```go
type OwnerReference struct {
    APIVersion         string    // 소유자 API 버전
    Kind               string    // 소유자 Kind
    Name               string    // 소유자 이름
    UID                types.UID // 소유자 UID
    Controller         *bool     // 컨트롤러 소유자 여부
    BlockOwnerDeletion *bool     // 소유자 삭제 시 블록 여부
}
```

**GC 동작**: 소유자가 삭제되면 종속 리소스도 자동 삭제 (가비지 컬렉션)

## API 그룹 구조

소스: `staging/src/k8s.io/api/` 디렉토리

```
/api/v1                        ← Core API (레거시, 그룹명 없음)
  Pod, Service, Node, ConfigMap, Secret, Namespace,
  PersistentVolume, PersistentVolumeClaim, Endpoints, ...

/apis/apps/v1                  ← Apps 그룹
  Deployment, ReplicaSet, StatefulSet, DaemonSet

/apis/batch/v1                 ← Batch 그룹
  Job, CronJob

/apis/networking.k8s.io/v1     ← Networking 그룹
  NetworkPolicy, Ingress, IngressClass

/apis/rbac.authorization.k8s.io/v1  ← RBAC 그룹
  Role, ClusterRole, RoleBinding, ClusterRoleBinding

/apis/autoscaling/v1,v2        ← Autoscaling 그룹
  HorizontalPodAutoscaler

/apis/storage.k8s.io/v1        ← Storage 그룹
  StorageClass, CSIDriver, CSINode, VolumeAttachment

/apis/admissionregistration.k8s.io/v1  ← Admission 그룹
  MutatingWebhookConfiguration, ValidatingWebhookConfiguration

/apis/apiextensions.k8s.io/v1  ← API Extensions
  CustomResourceDefinition
```

## ResourceVersion과 낙관적 동시성 제어

### ResourceVersion

- etcd의 revision을 문자열로 표현한 값
- 모든 객체에 자동 부여
- 업데이트 시 충돌 검사에 사용

```
1. Client A: GET Pod → resourceVersion: "100"
2. Client B: GET Pod → resourceVersion: "100"
3. Client A: PUT Pod (rv: "100") → 성공 → resourceVersion: "101"
4. Client B: PUT Pod (rv: "100") → 충돌 (409 Conflict)
   └─ 이유: 서버의 현재 rv는 "101"인데 "100" 기준으로 업데이트 시도
```

### Watch와 ResourceVersion

```
GET /api/v1/pods?watch=true&resourceVersion=100

→ 서버: rv=100 이후의 변경 이벤트를 스트리밍
→ ADDED   Pod/nginx   rv=101
→ MODIFIED Pod/nginx  rv=105
→ DELETED  Pod/old    rv=110
```

## Label과 Selector

### Label — 키-값 메타데이터

```yaml
metadata:
  labels:
    app: nginx
    env: production
    tier: frontend
```

### Selector — Label 기반 필터링

```
# Equality-based
app = nginx
env != staging

# Set-based
env in (production, staging)
tier notin (backend)
!canary  (canary 라벨이 없는 것)
```

### 사용 예시

```
Service.spec.selector:      { app: nginx }          → Label이 일치하는 Pod로 라우팅
Deployment.spec.selector:   { matchLabels: {app: nginx} }
NodeSelector:               { disktype: ssd }        → SSD 노드에만 스케줄
```

## Finalizer — 삭제 전 정리

```
1. 사용자: DELETE Pod (Finalizer: ["example.com/cleanup"])
2. API Server: DeletionTimestamp 설정, 실제 삭제 보류
3. 컨트롤러: 정리 작업 수행
4. 컨트롤러: Finalizer 제거 (PATCH)
5. API Server: 모든 Finalizer 제거됨 → 실제 삭제
```

## 핵심 상수

소스: `staging/src/k8s.io/api/core/v1/types.go`

```go
// 네임스페이스
const (
    NamespaceDefault   = "default"
    NamespaceAll       = ""
    NamespaceNodeLease = "kube-node-lease"
)

// Pod Phase
const (
    PodPending   PodPhase = "Pending"
    PodRunning   PodPhase = "Running"
    PodSucceeded PodPhase = "Succeeded"
    PodFailed    PodPhase = "Failed"
    PodUnknown   PodPhase = "Unknown"
)

// Service Type
const (
    ServiceTypeClusterIP    ServiceType = "ClusterIP"
    ServiceTypeNodePort     ServiceType = "NodePort"
    ServiceTypeLoadBalancer ServiceType = "LoadBalancer"
    ServiceTypeExternalName ServiceType = "ExternalName"
)

// Restart Policy
const (
    RestartPolicyAlways    RestartPolicy = "Always"
    RestartPolicyOnFailure RestartPolicy = "OnFailure"
    RestartPolicyNever     RestartPolicy = "Never"
)
```

## etcd 저장 경로

```
/registry/pods/{namespace}/{name}
/registry/services/specs/{namespace}/{name}
/registry/services/endpoints/{namespace}/{name}
/registry/deployments/{namespace}/{name}
/registry/nodes/{name}
/registry/namespaces/{name}
/registry/secrets/{namespace}/{name}
/registry/configmaps/{namespace}/{name}
```

키 생성 로직: `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` — `preparedKey` 구성 시 `/registry/` 접두사 + 리소스 종류 + 네임스페이스/이름 경로 조합
