# PoC 10: 라우트 레지스터

## 개요

Grafana의 계층적 라우트 등록 시스템을 시뮬레이션한다.
Grafana는 `pkg/api/api.go`에서 모든 HTTP 라우트를 등록하며,
그룹, 미들웨어, 경로 파라미터를 지원하는 자체 라우터를 사용한다.

## Grafana 실제 구조

라우트 등록은 다음 파일들에 구현되어 있다.

핵심 파일:
- `pkg/api/api.go` - registerRoutes() 함수에서 전체 API 라우트 등록
- `pkg/web/router.go` - 라우터 구현
- `pkg/api/routing/routing.go` - RouteRegister 인터페이스

## 라우트 등록 패턴

```go
// Grafana api.go 패턴
apiRoute := routing.Group("/api", middleware.ReqSignedIn)
apiRoute.Get("/dashboards/:uid", routing.Wrap(hs.GetDashboard))
apiRoute.Post("/dashboards/db", routing.Wrap(hs.PostDashboard))

adminRoute := apiRoute.Group("/admin", middleware.ReqGrafanaAdmin)
adminRoute.Get("/users", routing.Wrap(hs.AdminGetUsers))
```

## 시뮬레이션 내용

1. **RouteRegister 인터페이스**: Get/Post/Put/Delete/Group 메서드
2. **Route 구조체**: Method, Path, Handler, Middlewares
3. **Group**: 접두사 + 공유 미들웨어 상속
4. **경로 파라미터**: `:uid` 형태의 동적 세그먼트
5. **인증 레벨**: NoAuth, ReqSignedIn, ReqGrafanaAdmin
6. **라우트 매칭**: 정확 매칭 + 파라미터 추출
7. **등록된 라우트 테이블 출력**

## 실행

```bash
go run main.go
```

## 학습 포인트

- 계층적 라우트 그룹 패턴 (미들웨어 상속)
- HTTP 라우터의 경로 파라미터 매칭 알고리즘
- 인증 레벨별 미들웨어 적용 전략
- API 라우트 설계 패턴 (RESTful 구조)
