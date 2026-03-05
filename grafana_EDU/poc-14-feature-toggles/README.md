# PoC 14: 피처 토글

## 목적

Grafana의 기능 플래그(Feature Toggle) 시스템을 시뮬레이션한다.
기능별로 알파/베타/GA/폐기 단계를 관리하고, 런타임에 기능을 동적으로
활성화/비활성화하며, 조직이나 사용자 단위로 기능을 제어하는 패턴을 구현한다.

## Grafana 실제 구현 참조

Grafana는 `pkg/services/featuremgmt/` 패키지에서 피처 토글을 관리한다:

- **FeatureFlag**: 기능 이름, 설명, 단계, 의존성 등 메타데이터
- **FeatureManager**: 전역/컨텍스트별 기능 활성화 상태 관리
- **FeatureToggles 인터페이스**: `IsEnabled(ctx, flag)` — 기능 활성화 여부 확인

## 핵심 개념

### 기능 단계(Stage)

```
alpha → beta → GA → deprecated → 제거

  alpha:       개발 모드에서만 사용 가능, 프로덕션 비권장
  beta:        기본 비활성, 명시적 활성화 필요
  GA:          기본 활성, 안정적 기능
  deprecated:  폐기 예정, 경고 발생
```

### 기능 평가 흐름

```
IsEnabled(ctx, "featureX")
    │
    ▼
┌─────────────────────────┐
│ 1. 전역 설정 확인        │ ← startup config / CLI flag
│    enabled["featureX"]  │
└──────────┬──────────────┘
           │
           ▼
┌─────────────────────────┐
│ 2. 단계 제약 확인        │ ← alpha는 dev mode 필수
│    alpha && !devMode    │
│    → false              │
└──────────┬──────────────┘
           │
           ▼
┌─────────────────────────┐
│ 3. 컨텍스트 오버라이드   │ ← 조직/사용자별 설정
│    per-org / per-user   │
└──────────┬──────────────┘
           │
           ▼
┌─────────────────────────┐
│ 4. 의존성 확인           │ ← featureX requires featureY
│    featureY 비활성      │
│    → false              │
└──────────┬──────────────┘
           │
           ▼
        결과 반환
```

### 설정 우선순위

| 우선순위 | 소스 | 설명 |
|---------|------|------|
| 1 (최고) | 컨텍스트 오버라이드 | 조직/사용자별 동적 설정 |
| 2 | 런타임 토글 | API를 통한 런타임 변경 |
| 3 | 시작 설정 | 설정 파일/환경변수/CLI |
| 4 (최저) | 기본값 | 단계별 기본 활성화 상태 |

## 실행

```bash
go run main.go
```

## 출력 예시

```
=== Grafana 피처 토글 시뮬레이션 ===

[등록] 기능 플래그 7개 등록

┌─────────────────────┬──────┬──────────┬──────┬──────────────┐
│ 기능                 │ 단계 │ 기본활성 │ 현재 │ 의존성       │
├─────────────────────┼──────┼──────────┼──────┼──────────────┤
│ newNavigation       │ GA   │ true     │ ON   │ -            │
│ dashboardScene      │ beta │ false    │ OFF  │ newNavigation│
│ exploreMetrics      │ alpha│ false    │ OFF  │ -            │
│ correlations        │ GA   │ true     │ ON   │ -            │
│ nestedFolders       │ beta │ false    │ ON   │ -            │
│ publicDashboards    │ GA   │ true     │ ON   │ -            │
│ oldAlerts           │ depr │ false    │ OFF  │ -            │
└─────────────────────┴──────┴──────────┴──────┴──────────────┘
```

## 학습 포인트

1. **단계별 관리**: 기능의 성숙도에 따라 alpha/beta/GA/deprecated로 분류
2. **Dev Mode 제약**: alpha 기능은 개발 모드에서만 활성화 가능
3. **의존성 체인**: 기능 간 의존성으로 불완전한 활성화 방지
4. **컨텍스트 오버라이드**: 전역 설정과 별개로 조직/사용자별 제어 가능
5. **런타임 토글**: 재시작 없이 기능 활성화/비활성화
