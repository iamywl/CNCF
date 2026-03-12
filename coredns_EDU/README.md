# CoreDNS 교육 자료 (EDU)

## 프로젝트 개요

CoreDNS는 Go로 작성된 DNS 서버/포워더로, **플러그인 체인 아키텍처**를 통해 DNS 기능을 유연하게 조합할 수 있다. CNCF Graduated 프로젝트이며, Kubernetes의 기본 DNS 서버로 채택되어 클러스터 내 서비스 디스커버리의 핵심 역할을 수행한다.

| 항목 | 내용 |
|------|------|
| 프로젝트 | CoreDNS |
| 언어 | Go |
| 라이선스 | Apache License 2.0 |
| CNCF 등급 | Graduated |
| 소스코드 | `coredns/` |
| 핵심 의존성 | `github.com/miekg/dns`, `github.com/coredns/caddy` |
| 프로토콜 | DNS (UDP/TCP), DoT, DoH, DoH3, DoQ, gRPC |

## 핵심 특징

| 특징 | 설명 |
|------|------|
| 플러그인 체인 | 모든 기능이 플러그인으로 구현, 체인 패턴으로 조합 |
| Kubernetes 네이티브 | K8s 서비스/Pod DNS 자동 해석 (kubernetes 플러그인) |
| 다중 프로토콜 | DNS/DoT/DoH/DoH3/DoQ/gRPC 동시 지원 |
| 설정 간결성 | Corefile 기반 선언적 설정 (Caddy 파서 활용) |
| Zone 라우팅 | 쿼리 도메인 기반으로 적절한 Zone/플러그인 체인 선택 |
| 캐싱 | 양성/음성 캐시, 프리페치, 노화 서빙 지원 |
| 포워딩 | 업스트림 DNS 프록시, 헬스체크, 로드밸런싱 |
| 관측성 | Prometheus 메트릭, 헬스체크 엔드포인트, DNSTap |

## 아키텍처 개요

```
                          +-----------------+
                          |    Corefile      |
                          | (설정 파일)       |
                          +--------+--------+
                                   |
                                   v
                          +--------+--------+
                          |   Caddy 프레임워크 |
                          | (파싱/서버 관리)    |
                          +--------+--------+
                                   |
                    +--------------+--------------+
                    |              |              |
                    v              v              v
              +-----+----+  +-----+----+  +------+-----+
              |  Server   |  | ServerTLS|  | ServerQUIC |
              | (UDP/TCP) |  |  (DoT)   |  |   (DoQ)    |
              +-----+-----+ +-----+----+  +------+-----+
                    |              |              |
                    +--------------+--------------+
                                   |
                                   v
                    +-----------------------------+
                    |      ServeDNS()             |
                    |   (Zone 라우팅 멀티플렉서)     |
                    +-----------------------------+
                                   |
                    +--------------+--------------+
                    |              |              |
                    v              v              v
              +----+----+   +-----+----+   +-----+----+
              |example. |   | k8s.local|   |    .     |
              |com Zone |   |   Zone   |   | (root)   |
              +---------+   +----------+   +----------+
                    |
                    v
         +---+---+---+---+---+---+---+
         |log|cache|rewrite|forward|...|
         +---+---+---+---+---+---+---+
              플러그인 체인 (역순 조립)
```

## 소스코드 구조

```
coredns/
├── coredns.go              # main() 진입점 → coremain.Run()
├── coremain/run.go         # Run() 함수: 설정 로드 → caddy.Start
├── core/
│   ├── dnsserver/          # DNS 서버 엔진 (Server, Config, Register)
│   │   ├── server.go       # Server 구조체, ServeDNS(), Zone 라우팅
│   │   ├── config.go       # Config 구조체, FilterFunc
│   │   └── register.go     # dnsContext, InspectServerBlocks, MakeServers
│   └── plugin/zplugin.go   # 자동 생성된 플러그인 임포트
├── plugin/                 # 모든 플러그인 구현
│   ├── plugin.go           # Plugin/Handler 인터페이스 정의
│   ├── cache/              # 캐시 플러그인
│   ├── forward/            # 포워딩 플러그인
│   ├── kubernetes/         # K8s 서비스 디스커버리
│   ├── file/               # Zone 파일 플러그인
│   ├── log/                # 로깅 플러그인
│   ├── metrics/            # Prometheus 메트릭
│   └── ...                 # 50+ 플러그인
├── request/                # Request 래퍼, ScrubWriter
├── plugin.cfg              # 플러그인 실행 순서 정의
├── Makefile                # 빌드 시스템
└── test/                   # 통합 테스트
```

## 교육 자료 목차

### 기본 문서

| 번호 | 문서 | 내용 |
|------|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, 초기화 흐름, 플러그인 체인, 프로토콜 지원 |
| 02 | [데이터 모델](02-data-model.md) | DNS 메시지, RR 타입, Zone/Record, Request, Config |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | DNS 쿼리 처리, 캐시, 포워딩, K8s 조회 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리, Go 모듈, 빌드 시스템, 테스트 |
| 05 | [핵심 컴포넌트](05-core-components.md) | Server, Plugin, Cache, Forward, Kubernetes |
| 06 | [운영 가이드](06-operations.md) | 배포, 설정, 모니터링, 트러블슈팅, 성능 튜닝 |

### 심화 문서

| 번호 | 주제 | 핵심 내용 |
|------|------|----------|
| 07 | [플러그인 체인 아키텍처](07-plugin-chain-architecture.md) | Plugin/Handler, 체인 구축, 실행 순서 |
| 08 | [DNS 서버 엔진](08-dns-server-engine.md) | Server, ServeDNS, Zone 라우팅 |
| 09 | [Corefile 설정 파싱](09-corefile-parsing.md) | Caddy 통합, dnsContext, 설정 모델 |
| 10 | [Kubernetes 플러그인](10-kubernetes-plugin.md) | dnsController, Informer, 서비스 디스커버리 |
| 11 | [Forward 플러그인](11-forward-plugin.md) | 업스트림 프록시, 정책, 헬스체크 |
| 12 | [Cache 플러그인](12-cache-plugin.md) | 양성/음성 캐시, 프리페치, 노화 서빙 |
| 13 | [File 플러그인](13-file-plugin.md) | Zone 파일, 레코드 관리, Zone 전송 |
| 14 | [Request 처리](14-request-handling.md) | Request 래퍼, ScrubWriter, ResponseWriter |
| 15 | [메트릭과 헬스체크](15-metrics-health.md) | Prometheus, Health, 모니터링 |
| 16 | [로깅과 에러 처리](16-logging-errors.md) | Log, Errors, DNSTap |
| 17 | [프로토콜 지원](17-protocol-support.md) | DNS/DoT/DoH/gRPC/QUIC 다중 프로토콜 |
| 18 | [보안과 DNSSEC](18-security-dnssec.md) | TLS, DNSSEC, TSIG |

### PoC (Proof of Concept)

| 번호 | 주제 | 핵심 구현 |
|------|------|----------|
| poc-01 | [플러그인 체인](poc-01-plugin-chain/) | 체인 패턴 구현 |
| poc-02 | [DNS 서버](poc-02-dns-server/) | UDP/TCP DNS 서버 |
| poc-03 | [Zone 파일 파서](poc-03-zone-file-parser/) | Zone 파일 로딩/검색 |
| poc-04 | [DNS 포워더](poc-04-dns-forwarder/) | 업스트림 포워딩 |
| poc-05 | [캐시 서버](poc-05-cache-server/) | DNS 응답 캐싱 |
| poc-06 | [로드밸런서](poc-06-loadbalancer/) | 레코드 셔플링 |
| poc-07 | [서비스 디스커버리](poc-07-service-discovery/) | 서비스 -> DNS 매핑 |
| poc-08 | [EDNS 처리](poc-08-edns-handling/) | EDNS0 옵션 파싱 |
| poc-09 | [역방향 DNS](poc-09-reverse-dns/) | PTR 레코드 처리 |
| poc-10 | [DNS 메트릭](poc-10-dns-metrics/) | 쿼리 통계 수집 |
| poc-11 | [Corefile 파서](poc-11-corefile-parser/) | 설정 파일 파싱 |
| poc-12 | [Rewrite 규칙](poc-12-rewrite-rules/) | DNS 요청/응답 재작성 |
| poc-13 | [헬스체크](poc-13-healthcheck/) | 업스트림 헬스체크 |
| poc-14 | [와일드카드 매칭](poc-14-wildcard-matching/) | DNS 와일드카드 처리 |
| poc-15 | [TTL 관리](poc-15-ttl-management/) | DNS 레코드 TTL 처리 |
| poc-16 | [동시 DNS 리졸버](poc-16-concurrent-resolver/) | 병렬 쿼리 + 경쟁 |
