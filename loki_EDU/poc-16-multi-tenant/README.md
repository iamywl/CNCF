# PoC #16: 멀티테넌트 - 테넌트 격리, 리소스 제한, 데이터 분리

## 개요

Loki의 멀티테넌트 아키텍처(`pkg/ingester/instance.go`, `pkg/validation/limits.go`)를 시뮬레이션한다. 테넌트 ID 기반의 데이터 격리, per-tenant 리소스 제한, 런타임 설정 오버라이드를 재현한다.

## 아키텍처

```
HTTP 요청
│ X-Scope-OrgID: "tenant-a"
▼
┌──────────────────────────────────────────┐
│                Ingester                   │
│                                          │
│  ┌────────────────┐ ┌────────────────┐  │
│  │ Instance       │ │ Instance       │  │
│  │ (tenant-a)     │ │ (tenant-b)     │  │
│  │                │ │                │  │
│  │ streams:       │ │ streams:       │  │
│  │  {app="web"}→  │ │  {app="api"}→  │  │
│  │  {app="api"}→  │ │                │  │
│  │                │ │                │  │
│  │ rate limiter   │ │ rate limiter   │  │
│  │ stream limiter │ │ stream limiter │  │
│  └────────────────┘ └────────────────┘  │
│                                          │
│  RuntimeConfig (per-tenant overrides)    │
└──────────────────────────────────────────┘
```

## 테넌트 격리 메커니즘

```
요청 → X-Scope-OrgID 추출 → 테넌트별 Instance 조회
                                     │
                        ┌────────────┼────────────┐
                        ▼            ▼            ▼
                   레이블 검증   속도 제한 확인   스트림 수 확인
                        │            │            │
                        └────────────┴────────────┘
                                     │
                              ┌──────┴──────┐
                              ▼             ▼
                         Push 성공       Push 거부
```

## 실행 방법

```bash
go run main.go
```

## 시뮬레이션 내용

1. **테넌트별 리소스 제한 설정**: premium/free/standard 티어
2. **멀티테넌트 Ingester**: X-Scope-OrgID 기반 라우팅
3. **스트림 수 제한 테스트**: free-tenant의 5개 제한 초과
4. **쿼리 격리**: 테넌트 간 데이터 접근 차단
5. **런타임 설정 오버라이드**: 재시작 없이 제한 변경
6. **테넌트별 통계**: 스트림/엔트리/바이트/거부 카운트

## Loki 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/ingester/instance.go` | 테넌트별 Instance (스트림 관리, Push, Query) |
| `pkg/ingester/limiter.go` | 속도 제한, 스트림 수 제한 |
| `pkg/validation/limits.go` | TenantLimits 정의 (기본값, 오버라이드) |
| `pkg/runtime/config.go` | 런타임 설정 오버라이드 |
| `dskit/tenant` | X-Scope-OrgID 헤더 추출 |
