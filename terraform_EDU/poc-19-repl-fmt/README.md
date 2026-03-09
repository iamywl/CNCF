# PoC: Terraform REPL & fmt 시뮬레이션

## 개요

Terraform의 대화형 콘솔(`terraform console`)과 코드 포매터(`terraform fmt`)를 시뮬레이션한다.
REPL은 표현식을 즉시 평가하여 State 데이터를 탐색할 수 있게 하고,
fmt는 HCL 설정 파일을 표준 형식으로 자동 포매팅한다.

## 대응하는 Terraform 소스코드

| 이 PoC | Terraform 소스 | 설명 |
|--------|---------------|------|
| `Session` 구조체 | `internal/repl/session.go` | REPL 세션, Handle 메서드 |
| `FormatValue()` | `internal/repl/format.go` | cty.Value 포매팅 엔진 |
| `NeedsContinuation()` | `internal/repl/continuation.go` | 멀티라인 입력 감지 |
| `FormatHCL()` | `internal/command/fmt.go` | HCL 포매팅 (write/diff/check) |
| `unwrapInterpolation()` | `fmt.go` 내 인터폴레이션 언래핑 | `"${var.x}"` → `var.x` |
| `normalizeTypeExpression()` | `fmt.go` 내 타입 정규화 | `"string"` → `string` |
| `alignEquals()` | `hclwrite.Format()` | = 기호 정렬 |

## 구현 내용

### 1. REPL Session
- Scope 기반 표현식 평가 (변수, 리소스, 내장 함수)
- Handle 메서드: 빈 줄 무시, help, exit, 표현식 평가 분기
- 민감한 값(Sensitive) 마스킹

### 2. FormatValue 포매팅 엔진
- 재귀적 값 포매팅 (string, number, bool, list, map, tuple, object)
- 맵 키 알파벳 정렬 및 = 기호 패딩
- `tolist()`, `tomap()`, `tuple()` 타입 래핑

### 3. 멀티라인 입력 감지
- 괄호/중괄호/대괄호 깊이 추적
- 문자열 리터럴 내부 무시

### 4. terraform fmt
- 인터폴레이션 언래핑: `"${var.name}"` → `var.name`
- 타입 표현식 정규화: `type = "string"` → `type = string`
- = 기호 블록 단위 정렬
- diff/check/write 모드 시뮬레이션

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- `terraform console`은 무상태(Stateless) 평가를 수행하며, State를 변경하지 않는다
- FormatValue는 중첩된 cty.Value를 재귀적으로 포매팅하며, 민감한 값은 마스킹한다
- `terraform fmt`는 Go의 `gofmt`에서 영감을 받았으며, 코드 스타일 논쟁을 제거한다
- 인터폴레이션 언래핑은 Terraform 0.12+ 이후 권장되는 문법으로 자동 변환한다
