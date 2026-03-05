# gRPC-Go 교육 자료 (EDU)

## 프로젝트 개요

**gRPC-Go**는 Google이 개발한 고성능 RPC 프레임워크 [gRPC](https://grpc.io)의 Go 구현체이다.
HTTP/2 기반의 양방향 스트리밍, Protocol Buffers 직렬화, 플러거블 로드 밸런싱/이름 해석/인증을 지원한다.

- **저장소**: https://github.com/grpc/grpc-go
- **라이선스**: Apache License 2.0
- **언어**: Go
- **소스 경로**: `grpc-go/`

## 핵심 특징

| 특징 | 설명 |
|------|------|
| HTTP/2 트랜스포트 | 멀티플렉싱, 헤더 압축, 양방향 스트리밍 |
| Protocol Buffers | 기본 직렬화 포맷 (플러거블 코덱) |
| 4가지 RPC 패턴 | Unary, Server Streaming, Client Streaming, Bidirectional |
| 플러거블 밸런서 | pick_first, round_robin, weighted_round_robin, xDS 등 |
| 이름 해석 | DNS, passthrough, unix, xDS 리졸버 |
| 인터셉터 체인 | 클라이언트/서버 양방향 미들웨어 |
| 인증/보안 | TLS, ALTS, OAuth2, JWT, mTLS |
| 관측성 | channelz, stats handler, OpenTelemetry 연동 |
| xDS 지원 | 서비스 메시 (Envoy xDS 프로토콜) |

## EDU 문서 목차

### 기본문서

| # | 문서 | 내용 |
|---|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, 컴포넌트 관계, 초기화 흐름 |
| 02 | [데이터 모델](02-data-model.md) | 핵심 struct/interface, 서비스 정의 |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | RPC 호출 흐름, 커넥션 수립, 스트리밍 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 모듈 의존성, 빌드 시스템 |
| 05 | [핵심 컴포넌트](05-core-components.md) | Server, ClientConn, Transport, Stream 동작 원리 |
| 06 | [운영](06-operations.md) | 설정, 배포, 모니터링, 트러블슈팅 |

### 심화문서

| # | 문서 | 내용 |
|---|------|------|
| 07 | [트랜스포트](07-transport.md) | HTTP/2 프레이밍, 흐름 제어, 서버/클라이언트 트랜스포트 |
| 08 | [로드 밸런싱](08-balancer.md) | Balancer/Picker 인터페이스, pick_first, round_robin |
| 09 | [이름 해석](09-resolver.md) | Resolver 인터페이스, DNS/passthrough/unix 리졸버 |
| 10 | [인터셉터](10-interceptor.md) | 체이닝, Unary/Stream 인터셉터, 미들웨어 패턴 |
| 11 | [인코딩](11-encoding.md) | Codec/Compressor, Protocol Buffers, gzip 압축 |
| 12 | [인증/보안](12-credentials.md) | TLS, ALTS, PerRPCCredentials, SecurityLevel |
| 13 | [메타데이터](13-metadata.md) | MD 타입, 헤더/트레일러, 바이너리 헤더 |
| 14 | [상태 코드](14-status-codes.md) | 17개 gRPC 코드, Status 타입, 에러 처리 패턴 |
| 15 | [Keepalive](15-keepalive.md) | 핑/퐁, 유휴 관리, 커넥션 수명 |
| 16 | [Channelz](16-channelz.md) | 채널 관측성, 소켓 통계, 이벤트 트레이스 |
| 17 | [Stats](17-stats.md) | StatsHandler, RPC/커넥션 이벤트, OpenTelemetry |
| 18 | [xDS](18-xds.md) | xDS 프로토콜, LDS/RDS/CDS/EDS, 서비스 메시 |

### PoC (Proof of Concept)

| # | PoC | 핵심 개념 |
|---|-----|----------|
| 01 | [아키텍처](poc-01-architecture/) | 클라이언트-서버 기본 RPC 통신 |
| 02 | [데이터 모델](poc-02-data-model/) | ServiceDesc, MethodDesc 등록/디스패치 |
| 03 | [트랜스포트](poc-03-transport/) | HTTP/2 프레임 송수신 시뮬레이션 |
| 04 | [스트림](poc-04-stream/) | 멀티플렉스 스트림 관리 |
| 05 | [밸런서](poc-05-balancer/) | pick_first/round_robin 밸런서 |
| 06 | [리졸버](poc-06-resolver/) | 이름 해석 및 주소 갱신 |
| 07 | [인터셉터](poc-07-interceptor/) | 인터셉터 체이닝 |
| 08 | [인코딩](poc-08-encoding/) | 코덱 레지스트리 & 압축 |
| 09 | [인증](poc-09-credentials/) | TLS 핸드셰이크 시뮬레이션 |
| 10 | [메타데이터](poc-10-metadata/) | 헤더/트레일러 전파 |
| 11 | [상태 코드](poc-11-status-codes/) | gRPC 에러 처리 |
| 12 | [Keepalive](poc-12-keepalive/) | 핑/퐁 및 유휴 관리 |
| 13 | [Channelz](poc-13-channelz/) | 채널 진단 시스템 |
| 14 | [Stats](poc-14-stats/) | 메트릭 수집 |
| 15 | [흐름 제어](poc-15-flow-control/) | HTTP/2 윈도우 기반 흐름 제어 |
| 16 | [xDS](poc-16-xds/) | xDS 프로토콜 시뮬레이션 |

## 실행 방법

모든 PoC는 외부 의존성 없이 Go 표준 라이브러리만 사용한다:

```bash
cd poc-01-architecture
go run main.go
```
