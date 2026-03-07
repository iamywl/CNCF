# PoC 12: 표현식 평가(Expression Evaluation) 시뮬레이션

## 개요

Terraform의 표현식 평가 시스템을 시뮬레이션합니다. HCL 설정에서 사용되는 변수 참조, 함수 호출, 조건식, 문자열 보간 등을 평가하는 엔진을 구현합니다.

## 학습 목표

1. **Value 타입 시스템**: cty 기반의 String, Number, Bool, List, Map, Null, Unknown 타입
2. **변수 참조**: var.name, local.name 해석
3. **리소스 속성 참조**: aws_vpc.main.id 등의 교차 참조
4. **내장 함수**: upper, lower, length, format, join, lookup, element 등
5. **조건식**: condition ? true_val : false_val
6. **문자열 보간**: "Hello, ${var.name}!"
7. **Unknown 전파**: plan 단계에서의 미확정 값 처리

## Terraform 실제 코드 참조

| 개념 | 실제 파일 |
|------|----------|
| cty Value 타입 | `github.com/zclconf/go-cty/cty` |
| 표현식 평가 | `internal/lang/eval.go` |
| 내장 함수 | `internal/lang/functions.go` |
| Scope | `internal/lang/eval.go` |
| HCL 표현식 | `github.com/hashicorp/hcl/v2` |

## 구현 내용

### Value 타입 시스템

| 타입 | Go 표현 | HCL 예시 |
|------|---------|----------|
| TypeString | StrVal string | `"hello"` |
| TypeNumber | NumVal float64 | `42`, `3.14` |
| TypeBool | BoolVal bool | `true`, `false` |
| TypeList | ListVal []Value | `["a", "b"]` |
| TypeMap | MapVal map[string]Value | `{key = "val"}` |
| TypeNull | - | `null` |
| TypeUnknown | - | `(known after apply)` |

### 내장 함수

| 함수 | 설명 | 예시 |
|------|------|------|
| upper(s) | 대문자 변환 | `upper("hello")` -> `"HELLO"` |
| lower(s) | 소문자 변환 | `lower("HELLO")` -> `"hello"` |
| length(v) | 길이 반환 | `length("abc")` -> `3` |
| format(fmt, ...) | 포맷 문자열 | `format("%s-%d", "web", 1)` |
| join(sep, list) | 리스트 결합 | `join(", ", ["a", "b"])` |
| lookup(map, key, default) | 맵 조회 | `lookup(tags, "Name", "")` |
| element(list, idx) | 인덱스 접근 | `element(zones, 0)` |
| contains(list, val) | 포함 확인 | `contains(zones, "us-east-1a")` |
| concat(list...) | 리스트 연결 | `concat(list1, list2)` |
| coalesce(vals...) | 첫 non-null 값 | `coalesce(null, "default")` |

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. 변수/로컬 값 참조 해석
2. 리소스 속성 참조 (교차 참조)
3. 리터럴 값 평가
4. 내장 함수 호출
5. 조건식 평가
6. 문자열 보간
7. Unknown 값 전파 (plan 단계 시뮬레이션)
8. 오류 처리 (존재하지 않는 변수/함수)
