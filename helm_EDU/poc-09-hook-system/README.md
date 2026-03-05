# PoC-09: Helm Hook 시스템

## 개요

Helm Hook은 릴리스 라이프사이클의 특정 시점(install, upgrade, delete 등)에 실행되는 Kubernetes 리소스이다. 이 PoC는 Helm의 Hook 실행 엔진 핵심 로직을 시뮬레이션한다.

## 참조 소스코드

| 파일 | 설명 |
|------|------|
| `pkg/release/v1/hook.go` | Hook, HookEvent, HookDeletePolicy, HookPhase 타입 정의 |
| `pkg/action/hooks.go` | execHook, hookByWeight, deleteHookByPolicy 실행 엔진 |

## 핵심 개념

### 1. HookEvent (라이프사이클 이벤트)
- `pre-install` / `post-install`: 설치 전후
- `pre-upgrade` / `post-upgrade`: 업그레이드 전후
- `pre-delete` / `post-delete`: 삭제 전후
- `pre-rollback` / `post-rollback`: 롤백 전후
- `test`: `helm test` 실행 시

### 2. Weight 기반 정렬
- `sort.Stable(hookByWeight(hooks))` — 안정 정렬
- Weight 오름차순 (낮을수록 먼저 실행)
- 동일 Weight이면 이름 순서 유지

### 3. HookDeletePolicy (삭제 정책)
- `before-hook-creation` (기본값): 새 훅 생성 전 이전 리소스 삭제
- `hook-succeeded`: 실행 성공 시 리소스 삭제
- `hook-failed`: 실행 실패 시 리소스 삭제

### 4. 실행 흐름
```
이벤트 매칭 훅 필터링 → Weight 정렬 → 순차 실행:
  1. 기본 삭제 정책 설정
  2. before-hook-creation 처리
  3. 리소스 생성 + 완료 대기
  4. 성공/실패에 따른 삭제 정책 적용
```

## 실행

```bash
go run main.go
```

## 시뮬레이션 내용

1. 9종 HookEvent 타입과 3종 DeletePolicy 설명
2. Weight 기반 정렬 데모 (안정 정렬)
3. `helm install` 시나리오 (pre-install → 배포 → post-install)
4. 훅 실패 시나리오 및 에러 전파
5. CRD 훅 보호 메커니즘 (cascading garbage collection 방지)
6. 실행 결과 요약 및 아키텍처 다이어그램
