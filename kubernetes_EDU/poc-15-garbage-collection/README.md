# PoC-15: OwnerReference 기반 가비지 컬렉션 시뮬레이션

## 개요

Kubernetes의 OwnerReference 기반 가비지 컬렉션을 시뮬레이션한다. 의존성 그래프 구축, Foreground/Background/Orphan 삭제 정책, cascading 삭제를 구현한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| GarbageCollector | `pkg/controller/garbagecollector/garbagecollector.go` | 삭제 정책 디스패치, cascading 삭제 |
| 의존성 그래프 | `pkg/controller/garbagecollector/graph.go` (node 구조체) | dependents/owners 관계, beingDeleted 플래그 |
| GraphBuilder | `pkg/controller/garbagecollector/graph_builder.go` | 리소스 추가/삭제 시 그래프 갱신 |
| OwnerReference | `staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go` | UID 기반 소유 관계 |

## 삭제 정책 비교

```
┌─────────────────┬──────────────────────────┬──────────────────┐
│ 정책            │ 삭제 순서                │ Dependents       │
├─────────────────┼──────────────────────────┼──────────────────┤
│ Foreground      │ Dependents → Owner       │ 먼저 삭제        │
│ Background      │ Owner → Dependents       │ GC가 비동기 삭제 │
│ Orphan          │ Owner만 삭제             │ 살아남음 (고아)   │
└─────────────────┴──────────────────────────┴──────────────────┘
```

## 실행

```bash
go run main.go
```

## 데모 항목

1. **Foreground 삭제**: Deployment → ReplicaSet → Pod 역순 삭제
2. **Background 삭제**: Owner 즉시 삭제, GC가 dependent 비동기 삭제
3. **Orphan 삭제**: Owner만 삭제, dependents의 ownerRef 제거하여 고아 존속
4. **다중 소유자**: 공유 리소스에 owner가 하나라도 남으면 삭제 안 됨
5. **깊은 계층 Cascading**: CronJob → Job → Pod 3단계 전체 cascading
