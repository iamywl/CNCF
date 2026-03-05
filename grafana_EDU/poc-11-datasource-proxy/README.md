# PoC 11: 데이터소스 프록시

## 개요

Grafana의 데이터소스 리버스 프록시 패턴을 시뮬레이션한다.
Grafana는 프론트엔드에서 직접 데이터소스에 접근하지 않고,
백엔드 프록시를 통해 요청을 전달한다. 이를 통해 인증 정보를 안전하게 관리하고,
CORS 문제를 해결하며, 접근 제어를 수행한다.

## Grafana 실제 구조

데이터소스 프록시는 `pkg/api/datasource/` 디렉토리에 구현되어 있다.

핵심 파일:
- `pkg/api/datasource/proxy.go` - DataSourceProxy 구현
- `pkg/api/plugin_resource.go` - 플러그인 리소스 프록시
- `pkg/tsdb/` - 데이터소스별 쿼리 처리

## 프록시 흐름

```
브라우저 → Grafana Backend → 데이터소스 (Prometheus, Loki, InfluxDB 등)

1. 브라우저: GET /api/datasources/proxy/1/api/v1/query?query=up
2. Grafana: 데이터소스 ID=1 조회 → URL=http://prometheus:9090
3. Grafana: URL 재작성 → http://prometheus:9090/api/v1/query?query=up
4. Grafana: 인증 헤더 주입 (Basic Auth, OAuth 등)
5. Grafana: 요청 전달 → 응답 반환
```

## 시뮬레이션 내용

1. **DataSource 구조체**: UID, URL, Type, Access, BasicAuth 설정
2. **Director 함수**: URL 재작성, 인증 헤더 주입
3. **라우트 검증**: 화이트리스트 체크, 메서드 제한
4. **실제 HTTP 프록시**: 목 타겟 서버 → 프록시 서버 → 클라이언트
5. **요청/응답 헤더 변환**: 로그 출력
6. **접근 로그**: 프록시 통과 요청 기록

## 실행

```bash
go run main.go
```

## 학습 포인트

- 리버스 프록시 패턴 (Director 함수)
- HTTP 헤더 조작 (인증 주입, 호스트 재작성)
- 프록시 보안: 경로 화이트리스트, 메서드 제한
- Proxy vs Direct 접근 모드의 차이
