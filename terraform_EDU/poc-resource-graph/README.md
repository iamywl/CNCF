# PoC: Terraform 리소스 의존성 그래프 빌더 시뮬레이션

## 개요

Terraform이 리소스 정의에서 참조(Reference)를 분석하여
자동으로 의존성 그래프를 구축하는 과정을 시뮬레이션한다.

## 대응하는 Terraform 소스코드

| 이 PoC | Terraform 소스 | 설명 |
|--------|---------------|------|
| `ExtractReferences()` | `internal/lang/references.go` | 표현식에서 참조 추출 |
| `ApplyReferenceTransformer()` | `internal/terraform/transform_reference.go` | 참조 기반 간선 생성 |
| `ApplyTransitiveReduction()` | `internal/terraform/transform_transitive_reduction.go` | 이행적 축소 |
| `ResourceAddr` | `internal/addrs/resource.go` | 리소스 주소 체계 |

## 구현 내용

### 1. 참조(Reference) 추출
- 리소스 속성 값에서 다른 리소스에 대한 참조를 자동 탐지
- 예: `vpc_id = aws_vpc.main.id` → `aws_vpc.main`에 대한 의존성

### 2. ReferenceTransformer
- 추출된 참조를 기반으로 의존성 간선(Edge) 자동 생성
- `depends_on` 없이도 암묵적 의존 관계 파악

### 3. 이행적 축소 (Transitive Reduction)
- 불필요한 간접 간선 제거
- A → B → C 관계에서 A → C 직접 간선 제거
- 병렬 실행 기회 극대화

### 4. 위상 정렬
- 최종 그래프를 정렬하여 안전한 실행 순서 결정

## 실행 방법

```bash
go run main.go
```

## 의존성 그래프 구축 파이프라인

```
HCL 설정 파싱
     │
     ▼
┌────────────────────────┐
│ ReferenceTransformer   │ → 속성에서 참조 추출 → 간선 생성
└────────────────────────┘
     │
     ▼
┌────────────────────────┐
│ TransitiveReduction    │ → 이행적 간선 제거 → 그래프 최적화
└────────────────────────┘
     │
     ▼
┌────────────────────────┐
│ TopologicalSort        │ → 실행 순서 결정
└────────────────────────┘
```

## 핵심 포인트

- Terraform은 `depends_on` 없이도 속성 참조만으로 의존 관계를 자동 파악한다
- 이행적 축소를 통해 그래프를 최소화하여 병렬 처리 효율을 높인다
- 그래프 변환(Transform) 파이프라인으로 다양한 최적화를 체계적으로 적용한다
