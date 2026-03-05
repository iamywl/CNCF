# PoC-12: ClusterMesh 멀티클러스터 시뮬레이션

## 개요

Cilium ClusterMesh의 핵심 메커니즘을 시뮬레이션한다:
- KVStore (etcd) 기반 상태 게시/구독
- KVStoreMesh 캐싱 프록시 (cilium/state/ → cilium/cache/)
- GlobalServiceCache를 통한 멀티클러스터 서비스 병합
- ServiceAffinity 기반 클러스터 인식 라우팅
- 클러스터 간 Identity 동기화

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| ClusterMesh | `pkg/clustermesh/clustermesh.go` | 원격 클러스터 관리, SelectBackends |
| RemoteCluster | `pkg/clustermesh/remote_cluster.go` | 원격 KVStore 연결, SyncServices/SyncIdentities |
| GlobalServiceCache | `pkg/clustermesh/common/services.go` | OnUpdate/OnDelete, 서비스 병합 |
| ClusterService | `pkg/clustermesh/store/store.go` | KVStore 서비스 데이터 모델 (Shared, Backends) |
| KVStoreMesh | `pkg/clustermesh/kvstoremesh/kvstoremesh.go` | 원격→로컬 캐시 미러링 |
| ServiceAffinity | `pkg/clustermesh/selectbackends.go` | local/remote/none 백엔드 선택 |
| serviceMerger | `pkg/clustermesh/service_merger.go` | 로컬 LB와 글로벌 서비스 병합 |

## 핵심 아키텍처

```
┌──────────┐   KVStore 동기화   ┌──────────┐
│Cluster A │ ◄────────────────► │Cluster B │
│  (etcd)  │                    │  (etcd)  │
└────┬─────┘                    └────┬─────┘
     │                               │
┌────┴─────┐                    ┌────┴─────┐
│KVStoreMesh│                    │KVStoreMesh│
│(캐시)    │                    │(캐시)    │
└────┬─────┘                    └────┬─────┘
     │                               │
┌────┴─────┐                    ┌────┴─────┐
│  Agent   │                    │  Agent   │
│GlobalSvc │                    │GlobalSvc │
└──────────┘                    └──────────┘
```

## KVStore 키 구조

```
cilium/state/services/v1/<cluster>/<namespace>/<name>   → ClusterService JSON
cilium/state/identities/v1/<cluster>/<id>               → IdentityEntry JSON
cilium/state/nodes/v1/<cluster>/<node>                  → Node JSON
```

KVStoreMesh가 미러링할 때 접두사 변환:
```
cilium/state/  →  cilium/cache/
```

## GlobalServiceCache 병합

```
Cluster us-east: web-api → [10.1.1.1, 10.1.1.2]
Cluster eu-west: web-api → [10.2.1.1, 10.2.1.2, 10.2.1.3]
Cluster ap-south: web-api → [10.3.1.1]
                ↓ 병합 (Shared=true만)
Global: web-api → [10.1.1.1, 10.1.1.2, 10.2.1.1, 10.2.1.2, 10.2.1.3, 10.3.1.1]
```

## ServiceAffinity

| Affinity | 동작 |
|----------|------|
| `none` | 모든 클러스터의 백엔드를 사용 |
| `local` | 로컬 클러스터 우선 → 없으면 리모트 폴백 |
| `remote` | 리모트 클러스터 우선 → 없으면 로컬 폴백 |

## 실행

```bash
go run main.go
```

## 데모 항목

1. **서비스 게시**: 3개 클러스터에 서비스 등록, Shared=false는 멀티클러스터 제외
2. **Identity 동기화**: 클러스터별 Identity KVStore 게시
3. **KVStoreMesh 동기화**: cilium/state/ → cilium/cache/ 미러링
4. **ClusterMesh 서비스 디스커버리**: RemoteCluster 연결, GlobalServiceCache 병합
5. **Cluster-aware 라우팅**: ServiceAffinity (none/local/remote), 로컬 다운 시 폴백
6. **동적 업데이트**: 클러스터 제거, 스케일 아웃
7. **부하 분산**: 멀티클러스터 요청 분포 시뮬레이션
