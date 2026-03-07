# PoC: Terraform Plan 엔진 시뮬레이션

## 개요

Terraform의 Plan 프로세스를 시뮬레이션한다.
현재 상태(State)와 원하는 설정(Config)을 비교하여
어떤 리소스를 생성/변경/삭제할지 계산한다.

## 대응하는 Terraform 소스코드

| 이 PoC | Terraform 소스 | 설명 |
|--------|---------------|------|
| `PlanEngine.ComputePlan()` | `internal/terraform/node_resource_plan.go` | Plan 계산 |
| `diffResource()` | `internal/terraform/node_resource_plan_instance.go` | 리소스 diff |
| `Plan` 구조체 | `internal/plans/plan.go` | Plan 결과 |
| `ResourceChange` | `internal/plans/changes.go` | 리소스 변경 사항 |
| `Action` | `internal/plans/action.go` | 액션 타입 |

## 구현 내용

### 1. 액션 결정 로직
- **Create**: 설정에 있지만 상태에 없는 리소스
- **Delete**: 상태에 있지만 설정에 없는 리소스
- **Update**: 속성이 변경되었지만 in-place 변경 가능
- **Replace**: ForceNew 속성이 변경되어 재생성 필요
- **NoOp**: 변경 없음

### 2. ForceNew (Forces Replacement)
- 특정 속성(예: EC2의 ami)은 변경 시 리소스를 삭제 후 재생성해야 함
- 프로바이더 스키마에서 ForceNew=true로 정의됨
- Plan에서 `-/+` 기호로 표시

### 3. 속성별 Diff
- 각 속성의 이전 값/새 값 비교
- 추가(+), 삭제(-), 변경(~) 구분

## 실행 방법

```bash
go run main.go
```

## Plan 계산 흐름

```
현재 상태 (terraform.tfstate)          원하는 설정 (*.tf)
          │                                    │
          └──────────┬─────────────────────────┘
                     │
                     ▼
              ┌────────────┐
              │  Diff 엔진  │
              └────────────┘
                     │
            ┌────────┼────────┐
            ▼        ▼        ▼
         Create   Update   Delete
         (신규)   (변경)    (삭제)
                     │
                ForceNew?
               /         \
              ▼           ▼
          Replace     In-place
          (교체)      (현장 수정)
```

## 데모 시나리오

| 리소스 | 상태 | 설정 | 결과 |
|--------|------|------|------|
| aws_vpc.main | 있음 | 동일 | NoOp |
| aws_subnet.public | 있음 | 태그 추가 | Update |
| aws_instance.web | 있음 | AMI 변경 | Replace |
| aws_eip.web | 없음 | 있음 | Create |
| aws_security_group.deprecated | 있음 | 없음 | Delete |

## 핵심 포인트

- Plan은 실제 인프라를 변경하지 않고 변경 사항을 미리 확인하는 안전장치이다
- ForceNew 속성은 프로바이더 스키마에서 정의되며, Plan 단계에서 Replace로 결정된다
- `terraform plan` 출력의 `+`, `~`, `-`, `-/+` 기호가 각 액션을 나타낸다
