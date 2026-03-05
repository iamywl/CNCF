# PoC-16: xDS 프로토콜 시뮬레이션

## 개념

xDS(x Discovery Service)는 Envoy 프록시의 동적 설정 프로토콜로, gRPC는 이를 직접 지원하여 서비스 메시 없이도 동적 라우팅, 로드밸런싱, 엔드포인트 디스커버리를 수행할 수 있다.

### 리소스 계층 구조

```
LDS (Listener Discovery Service)
 │  "어떤 포트에서 어떤 프로토콜로 수신할 것인가?"
 │
 ▼
RDS (Route Discovery Service)
 │  "어떤 호스트/경로를 어떤 클러스터로 라우팅할 것인가?"
 │
 ▼
CDS (Cluster Discovery Service)
 │  "클러스터의 로드밸런싱 정책, 헬스체크는?"
 │
 ▼
EDS (Endpoint Discovery Service)
    "클러스터의 실제 백엔드 IP:port 목록은?"
```

### ADS (Aggregated Discovery Service)

```
xDS 클라이언트 ←─── 단일 gRPC 스트림 ───→ xDS 서버
                    (양방향 스트리밍)

장점:
- 모든 리소스 타입을 하나의 스트림으로 전달
- 리소스 간 순서 보장 (LDS 먼저, 그 다음 RDS, ...)
- 연결 수 감소
```

### ACK/NACK 메커니즘

| 요청 | version_info | error_detail | 의미 |
|------|-------------|-------------|------|
| 초기 구독 | "" | "" | 새 구독 시작 |
| ACK | "v1" | "" | v1 수락 |
| NACK | "v0" | "invalid" | v1 거부, v0 유지 |

NACK 시 클라이언트는 이전 버전을 유지하고, 서버는 수정된 설정을 다시 전송해야 한다.

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
xDS 프로토콜 시뮬레이션
========================================

[1] xDS 리소스 계층 구조
─────────────────────────
  LDS (Listener) → RDS (Route) → CDS (Cluster) → EDS (Endpoint)
  ...

[4] 서버 이벤트 로그
─────────────────────
  [서버] 리소스 업데이트: LDS/listener-80 → v1
  [서버] 리소스 업데이트: RDS/route-config-1 → v1
  [서버] 초기 구독: type=LDS, resources=[listener-80]
  [서버] ACK 수신: type=LDS, version=v1

[5] 클라이언트 이벤트 로그
──────────────────────────
  [클라이언트:node-1] 응답 수신: type=LDS, version=v1, resources=1개
  [클라이언트:node-1] 리소스 적용 완료: type=LDS, version=v1

[8] NACK 시뮬레이션
─────────────────────
  [클라이언트:node-2] 리소스 검증 실패: bad-cluster
  [서버] NACK 수신: type=CDS, error=invalid configuration
```

## 관련 소스

| 파일 | 설명 |
|------|------|
| `xds/` | xDS 관련 최상위 패키지 |
| `xds/internal/xdsclient/` | xDS 클라이언트 구현 |
| `xds/internal/balancer/` | xDS 기반 로드밸런서 |
| `internal/resolver/dns/` | DNS 리졸버 (비교 대상) |
