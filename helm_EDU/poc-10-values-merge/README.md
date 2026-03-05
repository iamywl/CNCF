# PoC-10: Helm Values 병합

## 개요

Helm의 Values 시스템은 차트 기본값, 사용자 오버라이드 파일, `--set` 플래그를 재귀적으로 병합하여 최종 템플릿 렌더링 입력을 생성한다. 이 PoC는 deep merge, `--set` 파싱, 서브차트 전파, JSON Schema 검증을 시뮬레이션한다.

## 참조 소스코드

| 파일 | 설명 |
|------|------|
| `pkg/chart/common/values.go` | Values 타입, GlobalKey, PathValue |
| `pkg/strvals/parser.go` | `--set` 문자열 파싱 (Parse, typedVal) |
| `pkg/chart/common/util/coalesce.go` | CoalesceValues, CoalesceTables, coalesceGlobals |

## 핵심 개념

### 1. Deep Merge 규칙
- 사용자 값(dst) > 차트 기본값(src)
- 맵은 재귀적으로 병합, 스칼라/배열은 덮어쓰기
- Coalesce 모드: null 값은 키 삭제 (`--set key=null`)
- Merge 모드: null 값 유지 (중간 처리용)

### 2. --set 파싱
- `a.b.c=value`: 점으로 중첩 맵 생성
- 타입 추론: "true"->bool, "123"->int64, "null"->nil
- `key[0].name=val`: 배열 인덱스 지원
- 쉼표로 복수 값 지정

### 3. 서브차트 값 전파
- `global` 키는 모든 서브차트에 전파
- 서브차트 로컬 값이 부모 전파값보다 우선

### 4. JSON Schema 검증
- `values.schema.json`으로 타입, 필수 필드, 범위, 열거형 검증

## 실행

```bash
go run main.go
```

## 시뮬레이션 내용

1. YAML Values deep merge (맵 재귀 병합 vs 스칼라 덮어쓰기)
2. `--set` 문자열 파싱 (점 경로, 타입 추론, 배열 인덱스, 이스케이프)
3. null 값 처리 차이 (Coalesce vs Merge)
4. 서브차트 global 키 전파
5. JSON Schema 검증 (타입, 범위, 필수, 열거형)
6. 전체 Values 파이프라인 다이어그램 및 시뮬레이션
