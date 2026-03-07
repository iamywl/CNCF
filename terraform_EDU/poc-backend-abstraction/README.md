# PoC 9: Backend 추상화 시뮬레이션

## 개요

Terraform의 백엔드(Backend) 시스템을 시뮬레이션합니다. 백엔드는 Terraform 상태(State)의 저장, 조회, 잠금을 담당하는 핵심 추상화 계층입니다.

## 학습 목표

1. **Backend 인터페이스 이해**: StateMgr, Workspaces, DeleteWorkspace의 역할
2. **StateMgr 인터페이스 이해**: State 읽기/쓰기/영속화/새로고침의 4단계 라이프사이클
3. **State 잠금**: 동시 접근 방지를 위한 Lock/Unlock 메커니즘
4. **워크스페이스**: 동일 구성으로 여러 환경(dev/staging/prod) 관리
5. **백엔드 마이그레이션**: 백엔드 간 상태 이전

## Terraform 실제 코드 참조

| 개념 | 실제 파일 |
|------|----------|
| Backend 인터페이스 | `internal/backend/backend.go` |
| StateMgr 인터페이스 | `internal/states/statemgr/statemgr.go` |
| Local 백엔드 | `internal/backend/local/backend.go` |
| S3 백엔드 | `internal/backend/remote-state/s3/backend.go` |
| Lock 정보 | `internal/states/statemgr/lock.go` |

## 구현 내용

### 백엔드 구현체

| 백엔드 | 저장소 | 잠금 방식 | 용도 |
|--------|--------|----------|------|
| InMemoryBackend | 메모리 | mutex 기반 | 테스트 |
| LocalBackend | 파일 시스템 | .lock 파일 | 개인 개발 |
| S3LikeBackend | 디렉토리(S3 시뮬레이션) | 파일(DynamoDB 시뮬레이션) | 팀 협업 |

### 주요 인터페이스

```
Backend
├── StateMgr(workspace) → StateMgr
├── Workspaces() → []string
└── DeleteWorkspace(name, force) → error

StateMgr
├── State() → *State
├── WriteState(state) → error
├── PersistState() → error
└── RefreshState() → error

Locker
├── Lock(info) → (lockID, error)
└── Unlock(id) → error
```

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. 각 백엔드(InMemory, Local, S3Like)로 동일한 작업 수행
2. 워크스페이스 생성 및 상태 기록
3. State 잠금/해제 테스트 (이중 잠금 충돌 포함)
4. 워크스페이스 삭제 (force 옵션)
5. 백엔드 간 상태 마이그레이션 시뮬레이션

## 핵심 설계 원리

- **인터페이스 분리**: Backend, StateMgr, Locker가 각각 독립적인 인터페이스
- **Write-then-Persist 패턴**: WriteState()는 메모리에만 기록, PersistState()에서 실제 저장
- **잠금 필수**: 팀 환경에서 동시 수정 방지
- **default 워크스페이스**: 삭제 불가, 항상 존재
