# PoC: Terraform HCL 파서 시뮬레이션

## 개요

Terraform의 설정 언어인 HCL(HashiCorp Configuration Language) 파서를 직접 구현한다.
토큰화(Lexing)부터 AST 구축(Parsing)까지의 전체 파이프라인을 시뮬레이션한다.

## 대응하는 Terraform 소스코드

| 이 PoC | 실제 코드 | 설명 |
|--------|----------|------|
| `Lexer` | `hcl/v2/hclsyntax/scan_tokens.go` | 토큰화 |
| `Parser` | `hcl/v2/hclsyntax/parser.go` | 구문 분석 |
| `Block` | `hcl/v2/hclsyntax/structure.go` → `Block` | 블록 구조 |
| `Module` | `internal/configs/module.go` → `Module` | 모듈 구조 |
| 참조 추출 | `internal/lang/references.go` | 리소스 간 참조 분석 |

## 구현 내용

### 1. 토큰화 (Lexing)
- HCL 소스 문자열을 토큰 스트림으로 변환
- 지원 토큰: 식별자, 문자열, 숫자, 불리언, 연산자, 괄호, 주석

### 2. 블록 파싱
- `resource`, `variable`, `output`, `provider`, `data`, `locals` 블록 인식
- 블록 라벨(type, name) 추출
- key = value 속성 파싱

### 3. 중첩 블록
- `lifecycle`, `provisioner` 등 중첩 블록 지원
- 재귀적 블록 파싱

### 4. 모듈 구축
- 파싱된 블록을 Module 구조체로 분류
- Resources, Variables, Outputs, Providers 등으로 그룹핑

### 5. 참조 추출
- 속성 값에서 다른 리소스 참조 탐지
- 변수 참조 (`var.xxx`) 탐지

## 실행 방법

```bash
go run main.go
```

## HCL 파싱 파이프라인

```
소스 코드 (.tf)
     │
     ▼
┌──────────┐
│  Lexer   │ → 토큰 스트림 (IDENT, STRING, EQUALS, ...)
└──────────┘
     │
     ▼
┌──────────┐
│  Parser  │ → AST (Block, Attribute, ...)
└──────────┘
     │
     ▼
┌──────────┐
│  Module  │ → 구조화된 설정 (Resources, Variables, ...)
└──────────┘
```

## 핵심 포인트

- HCL은 JSON보다 사람이 읽기 쉬운 설정 언어로 설계되었다
- 블록 기반 구조로 인프라 리소스를 선언적으로 정의한다
- 파싱 결과를 통해 리소스 간 의존 관계를 분석할 수 있다
