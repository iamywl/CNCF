# etcd 교육 자료 (EDU)

## 프로젝트 개요

etcd는 CNCF(Cloud Native Computing Foundation) 졸업 프로젝트로, 분산 시스템의 가장 중요한 데이터를 저장하는 신뢰할 수 있는 분산 키-값 저장소이다. Raft 합의 알고리즘을 사용하여 고가용성을 보장하며, Kubernetes의 핵심 데이터 저장소로 사용된다.

### 핵심 특징

| 특징 | 설명 |
|------|------|
| 분산 합의 | Raft 알고리즘으로 리더 선출과 로그 복제 |
| MVCC 저장소 | 다중 버전 동시성 제어로 시간 여행 쿼리 지원 |
| Watch 메커니즘 | 키 변경 이벤트를 실시간 스트리밍 |
| Lease 관리 | TTL 기반 키 자동 만료 |
| gRPC API | 6개 서비스 (KV, Watch, Lease, Cluster, Maintenance, Auth) |
| 선형 일관성 | Linearizable 읽기로 최신 데이터 보장 |
| RBAC 인증 | User/Role/Permission 기반 접근 제어 |
| 트랜잭션 | 원자적 비교-교환(Compare-And-Swap) 연산 |

### 아키텍처 한눈에 보기

```
┌──────────────────────────────────────────────────────────────────┐
│                         etcd Server                               │
│                                                                   │
│  ┌─────────────┐    ┌─────────────┐    ┌──────────────┐          │
│  │  gRPC API   │───→│  EtcdServer │───→│  Raft Node   │          │
│  │  (6 서비스)  │    │  (조율자)    │    │  (합의 엔진)  │          │
│  └─────────────┘    └──────┬──────┘    └──────┬───────┘          │
│                            │                   │                  │
│  ┌─────────────┐    ┌──────▼──────┐    ┌──────▼───────┐          │
│  │   Lessor    │    │   MVCC KV   │    │     WAL      │          │
│  │  (TTL 관리)  │    │  (다중 버전) │    │  (선행 로그)  │          │
│  └─────────────┘    └──────┬──────┘    └──────────────┘          │
│                            │                                      │
│  ┌─────────────┐    ┌──────▼──────┐    ┌──────────────┐          │
│  │  AuthStore  │    │   Backend   │    │  Snapshotter │          │
│  │  (RBAC)     │    │  (BoltDB)   │    │  (상태 전송)  │          │
│  └─────────────┘    └─────────────┘    └──────────────┘          │
└──────────────────────────────────────────────────────────────────┘
```

## 소스코드 정보

- **언어**: Go
- **저장소**: https://github.com/etcd-io/etcd
- **라이선스**: Apache License 2.0
- **소스 위치**: `/etcd/` (이 모노레포 내)

## 교육 자료 목차

### 기본 문서

| 번호 | 문서 | 내용 |
|------|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, 컴포넌트 관계, 초기화 흐름 |
| 02 | [데이터 모델](02-data-model.md) | MVCC 리비전 모델, KeyValue 구조, 트랜잭션 |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | Put, Range, Watch, Lease 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 빌드 시스템, 의존성 |
| 05 | [핵심 컴포넌트](05-core-components.md) | Raft, MVCC, Watch, Lease 동작 원리 |
| 06 | [운영 가이드](06-operations.md) | 배포, 설정, 모니터링, 트러블슈팅 |

### 심화 문서

| 번호 | 문서 | 내용 |
|------|------|------|
| 07 | [Raft 합의 엔진](07-raft-consensus.md) | Raft 노드, 선거, 로그 복제, ReadIndex |
| 08 | [MVCC 저장소](08-mvcc-store.md) | store 구조, 리비전 관리, 트랜잭션 처리 |
| 09 | [B-tree 인덱스](09-btree-index.md) | treeIndex, keyIndex, generation 모델 |
| 10 | [Watch 메커니즘](10-watch-mechanism.md) | synced/unsynced 워처, IntervalTree |
| 11 | [WAL & 스냅샷](11-wal-snapshot.md) | Write-Ahead Log, 스냅샷 저장/복구 |
| 12 | [BoltDB 백엔드](12-boltdb-backend.md) | 배치 트랜잭션, 읽기 최적화, Defrag |
| 13 | [Lease 시스템](13-lease-system.md) | TTL, KeepAlive, 체크포인트, Primary/Secondary |
| 14 | [gRPC API 계층](14-grpc-api.md) | 6개 서비스 핸들러, 인터셉터, 스트리밍 |
| 15 | [인증과 RBAC](15-auth-rbac.md) | AuthStore, 토큰, 권한 검사 |
| 16 | [클러스터 멤버십](16-cluster-membership.md) | ConfChange, 동적 확장, Learner 노드 |
| 17 | [컴팩션 & 조각 모음](17-compaction-defrag.md) | 리비전 컴팩션, 물리 조각 모음 |
| 18 | [선형 읽기 & 동시성](18-linearizable-read.md) | ReadIndex, 선형 읽기 루프, 동시성 패턴 |

### PoC (Proof of Concept)

| PoC | 주제 | 핵심 개념 |
|-----|------|----------|
| poc-01 | [MVCC 저장소](poc-01-mvcc-store/) | 리비전 기반 다중 버전 KV 저장소 |
| poc-02 | [B-tree 인덱스](poc-02-btree-index/) | 키-리비전 매핑 B-tree 인덱스 |
| poc-03 | [WAL 구현](poc-03-wal/) | Write-Ahead Log 기록/재생 |
| poc-04 | [Watch 메커니즘](poc-04-watch/) | 이벤트 감시 + synced/unsynced 분리 |
| poc-05 | [Raft 합의](poc-05-raft-consensus/) | 리더 선거 + 로그 복제 시뮬레이션 |
| poc-06 | [Lease TTL](poc-06-lease-ttl/) | TTL 기반 키 자동 만료 |
| poc-07 | [트랜잭션](poc-07-transaction/) | 원자적 비교-교환 트랜잭션 |
| poc-08 | [스냅샷](poc-08-snapshot/) | 상태 스냅샷 생성/복구 |
| poc-09 | [컴팩션](poc-09-compaction/) | 리비전 기반 히스토리 압축 |
| poc-10 | [RBAC 인증](poc-10-rbac-auth/) | 역할 기반 접근 제어 |
| poc-11 | [gRPC 서버](poc-11-grpc-server/) | KV/Watch gRPC 서비스 구현 |
| poc-12 | [클러스터 멤버십](poc-12-cluster-membership/) | 동적 멤버 추가/제거 |
| poc-13 | [배치 트랜잭션](poc-13-batch-tx/) | 버퍼링된 배치 커밋 |
| poc-14 | [IntervalTree](poc-14-interval-tree/) | 범위 워처 관리 자료구조 |
| poc-15 | [선형 읽기](poc-15-linearizable-read/) | ReadIndex 프로토콜 시뮬레이션 |
| poc-16 | [키 만료 알림](poc-16-lease-expiry/) | 힙 기반 만료 스케줄러 |
