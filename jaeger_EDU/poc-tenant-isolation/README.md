# PoC 14: 멀티테넌트 트레이스 격리 시뮬레이션

## 개요

Jaeger의 멀티테넌시(Multi-Tenancy) 시스템을 시뮬레이션합니다.
서로 다른 팀/조직이 동일한 Jaeger 인스턴스를 공유하면서도 트레이스 데이터가 완벽하게 격리되는 메커니즘을 구현합니다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `internal/tenancy/manager.go` | Manager, Guard 인터페이스, tenantList 화이트리스트 |
| `internal/tenancy/context.go` | WithTenant, GetTenant - context 기반 테넌트 전파 |
| `internal/tenancy/http.go` | ExtractTenantHTTPHandler - HTTP 미들웨어 |
| `internal/tenancy/grpc.go` | GetValidTenant, NewGuardingUnaryInterceptor - gRPC 인터셉터 |
| `internal/storage/v2/memory/tenant.go` | Tenant - 테넌트별 독립 스토리지 |

## 핵심 설계 원리

### 테넌트 검증 전략 (Guard 패턴)
```go
// 3가지 모드:
// 1. 테넌시 비활성화 → tenantDontCare (모든 요청 허용)
// 2. 테넌시 활성화 + 화이트리스트 없음 → tenantDontCare
// 3. 테넌시 활성화 + 화이트리스트 → tenantList (화이트리스트만 허용)
```

### Context 기반 테넌트 전파
```go
ctx = WithTenant(ctx, "team-alpha")     // 테넌트 저장
tenant := GetTenant(ctx)                 // 테넌트 추출
```

### HTTP 헤더 → Context 전파
```
HTTP 요청 → Header("x-tenant") → Manager.Valid() → WithTenant(ctx)
```

### gRPC Metadata → Context "업그레이드"
```
gRPC metadata → extractSingleTenant() → Valid() → WithTenant(ctx)
```

## 시뮬레이션 내용

1. **테넌트 매니저**: 화이트리스트 기반 테넌트 검증
2. **HTTP 미들웨어**: `httptest`를 사용한 요청별 테넌트 추출/검증
3. **데이터 격리 검증**: 테넌트 A가 테넌트 B의 데이터에 접근 불가 확인
4. **gRPC Metadata 전파**: metadata에서 테넌트 추출 후 context "업그레이드"
5. **동시 접근 격리**: 여러 테넌트가 동시에 쓰기/읽기 시에도 격리 유지

## 실행 방법

```bash
go run main.go
```

## 주요 출력

- 화이트리스트 기반 테넌트 유효성 검증 결과
- HTTP 요청별 테넌트 추출 및 응답 코드
- 교차 접근 시도 시 오류 메시지
- 같은 TraceID가 다른 테넌트에 독립 존재 확인
- gRPC metadata 전파 시뮬레이션 결과
- 동시 접근 시 격리 검증

## 핵심 인사이트

- 테넌트 정보는 `context.Context`를 통해 전파되며, 이는 Go의 관용적 패턴
- HTTP/gRPC 어느 경로로든 동일한 context 기반 테넌트 전파 메커니즘 사용
- Guard 패턴으로 검증 로직을 교체 가능 (화이트리스트, OIDC 등)
- 스토리지 레벨에서 테넌트별 완전 격리 (같은 TraceID도 테넌트별 독립)
