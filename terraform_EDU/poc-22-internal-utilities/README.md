# PoC: Terraform 내부 유틸리티 시뮬레이션

## 개요

Terraform 내부의 Promising(데드락 프리 Promise), Named Values(값 저장소),
Instance Expander(count/for_each 확장), JSON Plan 직렬화를 시뮬레이션한다.

## 대응하는 Terraform 소스코드

| 이 PoC | Terraform 소스 | 설명 |
|--------|---------------|------|
| `Promise` | `internal/promising/promise.go` | 비동기 Promise |
| `TaskRunner` | `internal/promising/task.go` | 태스크 스폰 |
| `Once` | `internal/promising/once.go` | 단일 실행 보장 |
| `NamedValuesState` | `internal/namedvals/namedvals.go` | 값 저장소 |
| `InstanceExpander` | `internal/instances/expander.go` | count/for_each 확장 |
| `JSONPlan` | `internal/command/jsonplan/plan.go` | JSON Plan 직렬화 |

## 구현 내용

### 1. Promising
- Promise 생명주기 (Unresolved → Resolved/Rejected)
- 자기 의존성 감지 (데드락 방지)
- 태스크 체이닝 (task1 결과 → task2 사용)

### 2. Once 패턴
- 함수를 한 번만 실행하고 결과를 캐시
- 에러도 캐시 (재시도 없음)

### 3. Named Values
- 변수, 로컬, 출력 값의 그래프 워크 저장소
- 모듈 경로 및 인스턴스 키 지원
- Placeholder (아직 미결정) 시스템

### 4. Instance Expander
- Single (단일), Count (정수), ForEach (맵) 확장 모드
- Unknown 모드 (Plan에서 아직 결정되지 않음)

### 5. JSON Plan 직렬화
- terraform show -json 출력 구조

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- Promising은 goroutine 기반 비동기 작업에서 데드락을 자동 감지한다
- Named Values는 그래프 워크 중 값을 안전하게 공유하는 저장소다
- Instance Expander는 count/for_each의 정적 확장을 담당한다
