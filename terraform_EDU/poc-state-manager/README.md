# PoC: Terraform State 관리 시뮬레이션

## 개요

Terraform의 State(상태) 관리 시스템을 시뮬레이션한다.
State는 실제 인프라와 설정 파일 사이의 매핑 정보를 저장하며,
Terraform이 인프라의 현재 상태를 파악하는 핵심 데이터이다.

## 대응하는 Terraform 소스코드

| 이 PoC | Terraform 소스 | 설명 |
|--------|---------------|------|
| `State` 구조체 | `internal/states/state.go` | 전체 상태 |
| `SyncState` | `internal/states/sync.go` | 동시성 안전 상태 접근 |
| `ResourceInstance` | `internal/states/instance_object.go` | 리소스 인스턴스 |
| `SerializeState()` | `internal/states/statefile/version4.go` | JSON 직렬화 |
| `FileLocker` | `internal/statemgr/filesystem.go` | 파일 기반 잠금 |
| `DeposeResourceInstance()` | `internal/states/sync.go` → `DeposeResourceInstanceObject()` | Deposed 처리 |

## 구현 내용

### 1. 상태 데이터 구조
- StateFile: 최상위 (version, serial, lineage)
- State → Module → Resource → ResourceInstance 계층
- 속성(Attributes)을 map[string]string으로 저장

### 2. SyncState (동시성 안전)
- RWMutex 기반 읽기/쓰기 잠금
- SetResourceInstance, GetResourceInstance, RemoveResourceInstance
- 병렬 apply 시 여러 goroutine이 동시에 상태 업데이트

### 3. JSON 직렬화/역직렬화
- State를 JSON v4 포맷으로 저장/로드
- Serial 번호로 상태 파일 버전 관리
- Lineage로 상태 파일의 동일성 확인

### 4. 상태 잠금 (State Locking)
- 파일 기반 잠금 (.tfstate.lock)
- 동시 실행 방지 (두 번째 apply/plan 차단)
- 잠금 정보: ID, Operation, Who, Created

### 5. Deposed 인스턴스
- create_before_destroy 시 사용
- 현재 인스턴스를 deposed로 전환 후 새 인스턴스 생성
- 새 인스턴스 성공 시 deposed 삭제

## 실행 방법

```bash
go run main.go
```

## State 계층 구조

```
StateFile
├── version: 4
├── serial: 1
├── lineage: "..."
└── State
    └── Modules
        ├── "root" (루트 모듈)
        │   └── Resources
        │       ├── "aws_vpc.main"
        │       │   └── Instances
        │       │       └── "current" → {id: "vpc-abc123", ...}
        │       ├── "aws_subnet.public"
        │       │   └── Instances
        │       │       └── "current" → {id: "subnet-def456", ...}
        │       └── "aws_instance.web"
        │           └── Instances
        │               ├── "current"     → {id: "i-new99999", ...}
        │               └── "deposed-001" → {id: "i-ghi789", ...}  (CBD)
        └── "module.network"
            └── Resources
                └── "aws_route_table.main"
```

## 핵심 포인트

- State는 terraform.tfstate 파일에 JSON으로 저장된다
- 원격 백엔드(S3, GCS 등) 사용 시 잠금은 DynamoDB, GCS Object Lock 등으로 구현된다
- Serial 번호가 상태 업데이트마다 증가하여 충돌을 감지한다
- create_before_destroy는 Deposed 메커니즘으로 다운타임 없이 리소스를 교체한다
