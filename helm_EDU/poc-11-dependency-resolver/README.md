# PoC-11: Helm 의존성 해석

## 개요

Helm의 의존성 시스템은 Chart.yaml의 `dependencies` 섹션에서 서브차트를 선언하고, SemVer 제약 조건으로 버전을 해석하여 Chart.lock 파일로 고정한다. 이 PoC는 SemVer 제약 해석, 의존성 트리 구축, Lock 파일 생성, condition/tags 평가를 시뮬레이션한다.

## 참조 소스코드

| 파일 | 설명 |
|------|------|
| `internal/resolver/resolver.go` | Resolver, Resolve, HashReq |
| `pkg/chart/v2/dependency.go` | Dependency, Lock 구조체 |
| `pkg/chart/v2/metadata.go` | Metadata (Dependencies 포함) |

## 핵심 개념

### 1. SemVer 제약 조건
- `^1.2.0`: caret - major 호환 (>=1.2.0, <2.0.0)
- `~1.2.0`: tilde - minor 호환 (>=1.2.0, <1.3.0)
- `>=1.0.0 <2.0.0`: 명시적 범위
- `1.2.x`: 와일드카드
- `*`: 모든 버전 허용

### 2. 의존성 해석 흐름
1. Chart.yaml dependencies 읽기
2. 리포지토리 인덱스에서 제약 만족 버전 검색
3. 내림차순 정렬된 버전 중 첫 매치 (최신) 선택
4. Chart.lock 생성 (digest로 변경 감지)

### 3. condition/tags 평가
- `condition`: values에서 불리언 값 조회 (우선)
- `tags`: values.tags에서 태그별 불리언 확인
- 둘 다 없으면 기본 활성화

## 실행

```bash
go run main.go
```

## 시뮬레이션 내용

1. SemVer 제약 조건 파싱 및 버전 매칭 (7종 패턴)
2. 의존성 트리 구축 및 Chart.lock 생성
3. condition/tags 기반 서브차트 활성화/비활성화
4. 의존성 해석 실패 시나리오
5. Lock 파일 digest 기반 변경 감지
6. 전체 아키텍처 다이어그램
