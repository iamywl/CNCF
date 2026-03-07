# PoC 16: 리소스 Diff 계산 시뮬레이션

## 개요

Terraform의 리소스 변경 사항(Diff) 계산 시스템을 시뮬레이션합니다. 현재 상태(prior state)와 계획된 상태(planned state)를 스키마 기반으로 비교하여 속성별 변경 유형을 결정하고, ForceNew/Sensitive/Nested Block 등의 특수 케이스를 처리합니다.

## 학습 목표

1. **ResourceSchema**: Required, Optional, Computed, ForceNew, Sensitive 속성 플래그
2. **ChangeAction**: Create, Update, Replace, Delete, Noop 결정 로직
3. **ForceNew**: 특정 속성 변경 시 리소스 교체(destroy + create) 트리거
4. **Sensitive**: 민감 속성 값 마스킹
5. **Nested Block**: 중첩 블록(ingress/egress 규칙 등) 비교
6. **Plan 출력**: Terraform plan 형식의 diff 포맷팅

## Terraform 실제 코드 참조

| 개념 | 실제 파일 |
|------|----------|
| ResourceInstanceChange | `internal/plans/changes.go` |
| 객체 변경 비교 | `internal/plans/objchange/` |
| Diff 포맷팅 | `internal/command/format.go` |
| Schema 정의 | `internal/providers/schema.go` |
| Action 타입 | `internal/plans/action.go` |

## 구현 내용

### 스키마 속성 플래그

| 플래그 | 설명 | 예시 |
|--------|------|------|
| Required | 필수 입력 | `ami`, `instance_type` |
| Optional | 선택 입력 | `tags`, `key_name` |
| Computed | 프로바이더 계산 | `id`, `private_ip` |
| ForceNew | 변경 시 교체 | `ami`, `subnet_id` |
| Sensitive | 값 마스킹 | `user_data`, `password` |

### 변경 액션

| 액션 | 기호 | 조건 |
|------|------|------|
| Create | `+` | old=nil, new!=nil |
| Update | `~` | 일반 속성 변경 |
| Replace | `-/+` | ForceNew 속성 변경 |
| Delete | `-` | old!=nil, new=nil |
| Noop | ` ` | 변경 없음 |

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. **리소스 생성**: 새 인스턴스 생성 (Computed 속성은 "known after apply")
2. **제자리 업데이트**: instance_type 변경 (ForceNew 아님)
3. **ForceNew 교체**: ami 변경 → 리소스 교체 (destroy + create)
4. **Sensitive 마스킹**: user_data 변경 시 값 숨김
5. **리소스 삭제**: 모든 속성 제거
6. **중첩 블록**: Security Group ingress 규칙 추가/제거
7. **변경 없음**: 동일 상태 비교 → Noop
8. **복합 변경**: 여러 속성 동시 변경 (ForceNew + Update + Sensitive)

## 핵심 설계 원리

- **스키마 기반 비교**: 속성의 메타데이터(ForceNew, Sensitive 등)에 따라 동작 결정
- **ForceNew 우선**: ForceNew 속성이 하나라도 변경되면 전체 리소스 교체
- **Computed 처리**: 프로바이더가 결정하는 속성은 "known after apply" 표시
- **블록 비교**: 중첩 블록은 별도의 비교 로직으로 추가/제거/변경 판별
