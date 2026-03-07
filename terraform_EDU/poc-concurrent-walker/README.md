# PoC 15: 동시성 그래프 워커(Concurrent Graph Walker) 시뮬레이션

## 개요

Terraform의 DAG 기반 동시성 그래프 워커를 시뮬레이션합니다. 리소스 의존성 그래프를 병렬로 워킹하면서 세마포어로 동시 실행 수를 제한하고, 에러를 의존 노드로 전파하는 패턴을 구현합니다.

## 학습 목표

1. **DAG 병렬 워킹**: 의존성이 충족된 노드부터 동시 실행
2. **세마포어 제어**: `parallelism` 옵션으로 동시 goroutine 수 제한
3. **에러 전파**: 부모 노드 실패 시 자식 노드 자동 건너뛰기
4. **실행 타이밍**: 병렬 실행 시 실제 성능 향상 측정
5. **타임라인 시각화**: ASCII 기반 실행 타임라인 표시

## Terraform 실제 코드 참조

| 개념 | 실제 파일 |
|------|----------|
| Walker | `internal/dag/walk.go` |
| Graph 구조체 | `internal/dag/dag.go` |
| Operation Walker | `internal/terraform/graph_walk_operation.go` |
| Parallelism 설정 | `terraform apply -parallelism=N` |

## 구현 내용

### Walker 동작 원리

```
1. 모든 노드에 goroutine 할당
2. 각 goroutine은 의존성(부모) 노드 완료 대기
3. 세마포어 획득 (동시 실행 제한)
4. 노드 실행 (리소스 생성/수정/삭제)
5. 세마포어 반환
6. 에러 시 자식 노드에 전파
```

### 주요 기능

| 기능 | 설명 |
|------|------|
| 병렬 실행 | 독립적인 노드를 동시에 처리 |
| 세마포어 | parallelism 값으로 동시 실행 수 제한 |
| 에러 전파 | 부모 실패 → 자식 SKIP |
| 타이밍 추적 | 시작/종료 시간 기록, 속도 향상 계산 |
| 타임라인 | ASCII 아트로 병렬 실행 시각화 |

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. **간단한 DAG**: 7개 노드, parallelism=4로 병렬 실행
2. **병렬성 비교**: parallelism=1(순차) vs parallelism=4(병렬) 성능 차이
3. **에러 전파**: 중간 노드 실패 시 의존 체인 전체 건너뛰기
4. **대규모 그래프**: 20개 노드 인프라 그래프 병렬 워킹
5. **병렬성 설정 비교**: parallelism 1/2/4/8에 따른 성능 변화

## 핵심 설계 원리

- **goroutine per node**: 각 노드에 goroutine 할당하여 자연스러운 의존성 대기
- **channel 기반 동기화**: 노드 완료를 channel로 알림
- **세마포어 패턴**: buffered channel로 동시 실행 수 제한
- **에러 격리**: 에러가 무관한 노드로 전파되지 않음 (독립 브랜치는 계속 실행)
