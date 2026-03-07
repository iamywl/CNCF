# PoC 14: moved/removed 블록 처리 시뮬레이션

## 개요

Terraform 1.1+의 `moved` 블록과 1.7+의 `removed` 블록 처리를 시뮬레이션합니다. 리소스 이름 변경, 모듈 간 이동 시 상태를 자동으로 업데이트하는 리팩토링 기능을 구현합니다.

## 학습 목표

1. **MoveStatement**: from/to 주소 쌍으로 리소스 이동 표현
2. **이동 체인 해석**: A->B, B->C를 A->C로 단순화
3. **충돌 감지**: 두 이동이 같은 대상을 가리키는 경우
4. **교차 모듈 이동**: 루트 → 모듈, 모듈 → 모듈 간 리소스 이전
5. **RemoveStatement**: 상태에서 제거 (인프라 유지 또는 삭제)
6. **멱등성**: 이미 이동된 리소스에 대한 재실행 안전성

## Terraform 실제 코드 참조

| 개념 | 실제 파일 |
|------|----------|
| MoveStatement | `internal/refactoring/move_statement.go` |
| 유효성 검증 | `internal/refactoring/move_validate.go` |
| 이동 실행 | `internal/refactoring/move_execute.go` |
| RemoveStatement | `internal/refactoring/remove_statement.go` |

## 구현 내용

### moved 블록

```hcl
moved {
  from = aws_instance.old_name
  to   = aws_instance.new_name
}
```

### removed 블록

```hcl
removed {
  from = aws_instance.legacy
  lifecycle {
    destroy = false  # 인프라 유지, Terraform 관리에서만 제외
  }
}
```

### 주요 기능

| 기능 | 설명 |
|------|------|
| 단순 이동 | 같은 모듈 내 이름 변경 |
| 이동 체인 | A->B, B->C 자동 해석 → A->C |
| 충돌 감지 | 두 move가 같은 대상을 가지면 오류 |
| 교차 모듈 | 루트↔모듈, 모듈↔모듈 간 이동 |
| 제거 (유지) | 상태에서만 제거, 인프라 유지 |
| 제거 (삭제) | 상태 제거 + 인프라 삭제 |
| 멱등성 | 소스 없으면 건너뛰기 |

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. 단순 리소스 이름 변경
2. 이동 체인 해석 (A->B->C)
3. 대상 충돌 감지
4. 루트 모듈 → 하위 모듈 이동
5. 모듈 간 이동 (module.old → module.new)
6. removed 블록 (destroy=false/true)
7. 복합 시나리오 (이동 + 체인 + 제거)
8. 멱등성 테스트 (이미 이동된 리소스)
