# etcd 코드 구조

## 1. 개요

etcd는 Go 모듈 기반의 모노레포 구조로, 여러 개의 독립 모듈이 하나의 저장소에 공존한다. 핵심 서버 코드(`server/`), API 정의(`api/`), CLI 도구(`etcdctl/`, `etcdutl/`), 클라이언트 라이브러리(`client/`) 등으로 명확히 분리되어 있다.

## 2. 최상위 디렉토리 구조

```
etcd/
├── api/                    # API 정의 (protobuf, Go 타입)
│   ├── etcdserverpb/       # gRPC 서비스 정의 (rpc.proto)
│   ├── mvccpb/             # MVCC 타입 (KeyValue, Event)
│   ├── authpb/             # 인증 타입 (User, Role, Permission)
│   ├── membershippb/       # 멤버십 타입
│   ├── versionpb/          # 버전 정보
│   └── v3rpc/rpctypes/     # RPC 에러 타입
│
├── server/                 # 핵심 서버 코드
│   ├── etcdmain/           # 진입점 (main.go, etcd.go)
│   ├── embed/              # 임베드 서버 (Etcd struct, Config)
│   ├── etcdserver/         # EtcdServer 핵심 로직
│   │   ├── api/            # 내부 API 계층
│   │   │   ├── v3rpc/      # gRPC 핸들러 (key.go, watch.go, lease.go)
│   │   │   ├── rafthttp/   # Raft HTTP 전송
│   │   │   ├── membership/ # 클러스터 멤버십
│   │   │   ├── snap/       # 스냅샷 관리
│   │   │   └── etcdhttp/   # HTTP 핸들러
│   │   ├── apply/          # Raft 적용 로직
│   │   ├── txn/            # 트랜잭션 처리
│   │   ├── cindex/         # Consistent Index
│   │   ├── server.go       # EtcdServer 구조체
│   │   ├── raft.go         # raftNode 래퍼
│   │   └── v3_server.go    # v3 API 구현
│   ├── storage/            # 저장소 계층
│   │   ├── mvcc/           # MVCC 키-값 저장소
│   │   ├── backend/        # BoltDB 백엔드
│   │   ├── wal/            # Write-Ahead Log
│   │   ├── schema/         # 스토리지 스키마
│   │   └── datadir/        # 데이터 디렉토리 관리
│   ├── auth/               # RBAC 인증
│   ├── lease/              # Lease 관리
│   ├── config/             # 서버 설정 (ServerConfig)
│   └── proxy/              # 프록시 (grpcproxy, httpproxy)
│
├── client/                 # 클라이언트 라이브러리
│   ├── v3/                 # v3 클라이언트 (Go SDK)
│   └── pkg/                # 클라이언트 공통 유틸리티
│
├── etcdctl/                # CLI 도구 (etcdctl)
│   └── ctlv3/              # v3 명령어 구현
│
├── etcdutl/                # 유틸리티 도구 (etcdutl)
│   ├── snapshot/           # 스냅샷 관리
│   └── etcdutl/            # 진입점
│
├── pkg/                    # 공유 유틸리티 패키지
│   ├── wait/               # 대기 레지스트리
│   ├── schedule/           # FIFO 스케줄러
│   ├── idutil/             # ID 생성기
│   ├── contention/         # 경합 감지
│   └── notify/             # 알림 메커니즘
│
├── contrib/                # 기여 코드
│   ├── raftexample/        # Raft 사용 예제
│   ├── lock/               # 분산 잠금 예제
│   └── systemd/            # systemd 유닛 파일
│
├── tests/                  # 테스트
│   ├── integration/        # 통합 테스트
│   ├── e2e/                # E2E 테스트
│   ├── robustness/         # 견고성 테스트
│   └── common/             # 공통 테스트 유틸
│
├── tools/                  # 개발 도구
│   ├── benchmark/          # 벤치마크 도구
│   ├── etcd-dump-db/       # DB 덤프
│   └── etcd-dump-logs/     # 로그 덤프
│
├── Documentation/          # 문서
│   ├── contributor-guide/  # 기여 가이드
│   ├── dev-guide/          # 개발 가이드
│   └── etcd-internals/     # 내부 구조 문서
│
├── hack/                   # 빌드/배포 스크립트
│   ├── kubernetes-deploy/  # K8s 배포
│   ├── tls-setup/          # TLS 설정
│   └── patch/              # 패치 스크립트
│
├── go.mod                  # 메인 모듈
├── Makefile                # 빌드 시스템
├── CHANGELOG/              # 변경 이력
└── logos/                  # 로고 이미지
```

## 3. Go 모듈 구조

etcd는 하나의 저장소에 여러 Go 모듈이 공존하는 **모노레포** 구조이다:

```
go.etcd.io/etcd/v3                    # 메인 모듈
go.etcd.io/etcd/api/v3               # API 타입 정의
go.etcd.io/etcd/server/v3            # 서버 구현
go.etcd.io/etcd/client/v3            # 클라이언트 SDK
go.etcd.io/etcd/client/pkg/v3        # 클라이언트 유틸리티
go.etcd.io/etcd/etcdctl/v3           # CLI 도구
go.etcd.io/etcd/etcdutl/v3           # 유틸리티 도구
go.etcd.io/etcd/pkg/v3               # 공유 패키지
go.etcd.io/etcd/tests/v3             # 테스트
```

**모듈 의존성 관계:**

```
                  api/v3
                 ↗     ↖
client/v3  ←  server/v3  →  go.etcd.io/raft/v3
                 ↓
             go.etcd.io/bbolt
```

## 4. 핵심 디렉토리 상세

### 4.1 server/etcdserver/ — 서버 핵심

| 파일 | 줄 수 (약) | 역할 |
|------|-----------|------|
| server.go | 2000+ | EtcdServer 구조체, NewServer(), run(), Start() |
| raft.go | 500+ | raftNode 래퍼, start(), apply() |
| v3_server.go | 800+ | Range(), Put(), DeleteRange(), Txn(), Compact() |
| bootstrap.go | 500+ | 부트스트랩 (WAL, 스냅샷, 클러스터 초기화) |

### 4.2 server/storage/mvcc/ — MVCC 저장소

| 파일 | 역할 |
|------|------|
| kvstore.go | store 구조체, Compact(), restore() |
| kvstore_txn.go | storeTxnRead, storeTxnWrite (트랜잭션 구현) |
| kv.go | KV, TxnRead, TxnWrite 인터페이스 |
| watchable_store.go | watchableStore, syncWatchersLoop, syncVictimsLoop |
| watcher.go | watcher 구조체, WatchStream |
| watcher_group.go | watcherGroup, IntervalTree 기반 범위 워처 |
| index.go | treeIndex (B-tree 인덱스) |
| key_index.go | keyIndex, generation (다중 버전 관리) |
| revision.go | Revision{Main, Sub} 구조체 |
| kv_view.go | readView, writeView (읽기/쓰기 뷰) |

### 4.3 server/storage/backend/ — BoltDB 백엔드

| 파일 | 역할 |
|------|------|
| backend.go | backend 구조체, 배치 주기/제한 관리 |
| batch_tx.go | batchTx, 배치 쓰기 트랜잭션 |
| read_tx.go | readTx, 공유/개별 읽기 트랜잭션 |
| hooks.go | 트랜잭션 후크 |

### 4.4 server/storage/wal/ — Write-Ahead Log

| 파일 | 역할 |
|------|------|
| wal.go | WAL 구조체, Create(), Save(), Cut() |
| encoder.go | 레코드 인코딩 (CRC32) |
| decoder.go | 레코드 디코딩 + 검증 |
| file_pipeline.go | 비동기 파일 사전 할당 |
| repair.go | 손상된 WAL 복구 |

### 4.5 server/etcdserver/api/v3rpc/ — gRPC 핸들러

| 파일 | 역할 |
|------|------|
| key.go | kvServer: Range, Put, DeleteRange, Txn, Compact |
| watch.go | watchServer, serverWatchStream: recvLoop, sendLoop |
| lease.go | LeaseServer: Grant, Revoke, KeepAlive |
| maintenance.go | maintenanceServer: Alarm, Status, Defrag, Snapshot |
| member.go | 클러스터 멤버 관리 RPC |
| auth.go | 인증 RPC 핸들러 |
| interceptor.go | gRPC 인터셉터 (인증, 제한) |
| grpc.go | gRPC 서버 등록 |

### 4.6 server/auth/ — RBAC 인증

| 파일 | 역할 |
|------|------|
| store.go | AuthStore 인터페이스, authStore 구현 |
| jwt.go | JWT 토큰 공급자 |
| simple_token.go | Simple 토큰 공급자 |
| range_perm_cache.go | 범위 권한 캐시 |

### 4.7 server/lease/ — Lease 관리

| 파일 | 역할 |
|------|------|
| lessor.go | Lessor 인터페이스, lessor 구현, Grant/Revoke/Renew |
| lease.go | Lease 구조체, TTL/만료 관리 |
| lease_queue.go | 만료 힙 (LeaseQueue) |

## 5. 빌드 시스템

### 5.1 Makefile

```makefile
# 주요 타겟
build:          # etcd, etcdctl, etcdutl 바이너리 빌드
test:           # 단위 테스트 실행
test-integration: # 통합 테스트
test-e2e:       # E2E 테스트
fmt:            # gofmt 적용
vet:            # go vet 실행
lint:           # golangci-lint 실행
proto:          # protobuf 코드 생성
```

### 5.2 go.mod 주요 의존성

| 의존성 | 용도 |
|--------|------|
| `go.etcd.io/raft/v3` | Raft 합의 알고리즘 |
| `go.etcd.io/bbolt` | 임베드 KV 저장소 (BoltDB 포크) |
| `google.golang.org/grpc` | gRPC 서버/클라이언트 |
| `go.uber.org/zap` | 구조화 로깅 |
| `golang.org/x/crypto` | bcrypt 등 암호화 |
| `github.com/prometheus/client_golang` | 메트릭 수집 |
| `go.opentelemetry.io/otel` | 분산 트레이싱 |
| `github.com/coreos/go-semver` | Semantic Versioning |
| `github.com/spf13/cobra` | CLI 프레임워크 (etcdctl) |
| `github.com/google/btree` | B-tree 인덱스 |
| `sigs.k8s.io/json` | JSON 직렬화 |

## 6. Protobuf 정의

### 6.1 주요 Proto 파일

```
api/
├── etcdserverpb/
│   ├── rpc.proto           # 6개 gRPC 서비스 + 메시지
│   ├── etcdserver.proto    # 내부 Raft 요청 타입
│   └── raft_internal.proto # InternalRaftRequest
├── mvccpb/
│   └── kv.proto            # KeyValue, Event
├── authpb/
│   └── auth.proto          # User, Role, Permission
├── membershippb/
│   └── membership.proto    # ClusterVersionSet
└── versionpb/
    └── version.proto       # ClusterVersionSetRequest
```

### 6.2 gRPC 서비스 정의

```protobuf
// api/etcdserverpb/rpc.proto

service KV {
    rpc Range(RangeRequest) returns (RangeResponse);
    rpc Put(PutRequest) returns (PutResponse);
    rpc DeleteRange(DeleteRangeRequest) returns (DeleteRangeResponse);
    rpc Txn(TxnRequest) returns (TxnResponse);
    rpc Compact(CompactionRequest) returns (CompactionResponse);
}

service Watch {
    rpc Watch(stream WatchRequest) returns (stream WatchResponse);
}

service Lease {
    rpc LeaseGrant(LeaseGrantRequest) returns (LeaseGrantResponse);
    rpc LeaseRevoke(LeaseRevokeRequest) returns (LeaseRevokeResponse);
    rpc LeaseKeepAlive(stream LeaseKeepAliveRequest) returns (stream LeaseKeepAliveResponse);
    rpc LeaseTimeToLive(LeaseTimeToLiveRequest) returns (LeaseTimeToLiveResponse);
    rpc LeaseLeases(LeaseLeasesRequest) returns (LeaseLeasesResponse);
}

service Cluster {
    rpc MemberAdd(MemberAddRequest) returns (MemberAddResponse);
    rpc MemberRemove(MemberRemoveRequest) returns (MemberRemoveResponse);
    rpc MemberUpdate(MemberUpdateRequest) returns (MemberUpdateResponse);
    rpc MemberList(MemberListRequest) returns (MemberListResponse);
    rpc MemberPromote(MemberPromoteRequest) returns (MemberPromoteResponse);
}

service Maintenance {
    rpc Alarm(AlarmRequest) returns (AlarmResponse);
    rpc Status(StatusRequest) returns (StatusResponse);
    rpc Defragment(DefragmentRequest) returns (DefragmentResponse);
    rpc Hash(HashRequest) returns (HashResponse);
    rpc HashKV(HashKVRequest) returns (HashKVResponse);
    rpc Snapshot(SnapshotRequest) returns (stream SnapshotResponse);
    rpc MoveLeader(MoveLeaderRequest) returns (MoveLeaderResponse);
    rpc Downgrade(DowngradeRequest) returns (DowngradeResponse);
}

service Auth {
    rpc AuthEnable(AuthEnableRequest) returns (AuthEnableResponse);
    rpc AuthDisable(AuthDisableRequest) returns (AuthDisableResponse);
    rpc AuthStatus(AuthStatusRequest) returns (AuthStatusResponse);
    rpc Authenticate(AuthenticateRequest) returns (AuthenticateResponse);
    // UserAdd, UserGet, UserList, UserDelete, UserChangePassword
    // UserGrantRole, UserRevokeRole
    // RoleAdd, RoleGet, RoleList, RoleDelete
    // RoleGrantPermission, RoleRevokePermission
}
```

## 7. 데이터 디렉토리 구조

etcd가 실행 중 생성하는 데이터 디렉토리:

```
data-dir/
└── member/
    ├── snap/               # 스냅샷 파일
    │   ├── 0000000000000001-0000000000000001.snap
    │   └── db              # BoltDB 파일 (스냅샷 시점)
    └── wal/                # Write-Ahead Log 파일
        ├── 0000000000000000-0000000000000000.wal
        └── 0000000000000001-0000000000010000.wal
```

| 디렉토리/파일 | 용도 |
|-------------|------|
| `member/snap/` | Raft 스냅샷 (상태 전송, 장애 복구) |
| `member/snap/db` | BoltDB 스냅샷 |
| `member/wal/` | WAL 세그먼트 (64MB씩 사전 할당) |

## 8. 테스트 구조

```
tests/
├── integration/           # 통합 테스트
│   ├── clientv3/          # v3 클라이언트 테스트
│   ├── embed/             # 임베드 서버 테스트
│   └── v3_*_test.go       # v3 API 테스트
├── e2e/                   # End-to-End 테스트
│   ├── ctl_v3_*_test.go   # etcdctl 테스트
│   └── cluster_test.go    # 클러스터 테스트
├── robustness/            # 견고성 테스트 (Jepsen 스타일)
│   ├── model/             # 모델 기반 검증
│   └── traffic/           # 트래픽 패턴
└── common/                # 공통 테스트 유틸리티
```

## 9. 패키지 의존성 그래프

```
etcdctl (CLI)
  └─ client/v3
      └─ api/v3 (protobuf 타입)
          └─ google.golang.org/grpc

server/v3 (서버)
  ├─ api/v3
  ├─ go.etcd.io/raft/v3 (Raft 합의)
  ├─ go.etcd.io/bbolt (BoltDB)
  ├─ server/storage/mvcc
  │   ├─ server/storage/backend (BoltDB 래퍼)
  │   └─ github.com/google/btree (인덱스)
  ├─ server/storage/wal
  ├─ server/lease
  ├─ server/auth
  └─ server/etcdserver/api/rafthttp
      └─ go.etcd.io/raft/v3
```

## 10. 요약

```
┌──────────────────────────────────────────────────────┐
│                etcd 코드 구조 요약                      │
├──────────────────────────────────────────────────────┤
│                                                       │
│  모듈 구조: 모노레포 + 다중 Go 모듈                      │
│                                                       │
│  api/    → Protobuf 정의 (gRPC 서비스, 메시지)          │
│  server/ → 핵심 서버 (Raft, MVCC, WAL, Auth, Lease)    │
│  client/ → Go 클라이언트 SDK                            │
│  etcdctl/→ CLI 도구 (cobra 기반)                       │
│                                                       │
│  핵심 의존성: raft/v3, bbolt, grpc, zap               │
│  빌드: Makefile + go build                            │
│  테스트: unit + integration + e2e + robustness        │
│                                                       │
└──────────────────────────────────────────────────────┘
```
