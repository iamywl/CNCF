# CoreDNS 시퀀스 다이어그램

## 1. DNS 쿼리 처리 흐름

CoreDNS가 클라이언트의 DNS 쿼리를 받아 응답을 반환하는 전체 흐름이다.

```mermaid
sequenceDiagram
    participant Client as DNS 클라이언트
    participant Listener as UDP/TCP Listener
    participant Server as Server.ServeDNS()
    participant ScrubW as ScrubWriter
    participant Chain as pluginChain
    participant Plugin1 as log 플러그인
    participant Plugin2 as cache 플러그인
    participant Plugin3 as forward 플러그인
    participant Upstream as 업스트림 DNS

    Client->>Listener: DNS 쿼리 (UDP/TCP)
    Listener->>Server: dns.HandlerFunc 호출

    Note over Server: 1. Question 섹션 유효성 검증
    Note over Server: 2. EDNS 버전 체크
    Server->>ScrubW: NewScrubWriter(r, w) 래핑

    Note over Server: 3. QNAME으로 Zone 매칭<br/>(longest match)
    Note over Server: 4. FilterFuncs 통과 확인

    Server->>Chain: pluginChain.ServeDNS(ctx, w, r)
    Chain->>Plugin1: log.ServeDNS()
    Note over Plugin1: 쿼리 정보 기록 시작
    Plugin1->>Plugin2: NextOrFailure → cache.ServeDNS()
    Note over Plugin2: 캐시 조회 (hash key)

    alt 캐시 미스
        Plugin2->>Plugin3: NextOrFailure → forward.ServeDNS()
        Plugin3->>Upstream: DNS 쿼리 포워딩
        Upstream-->>Plugin3: DNS 응답
        Plugin3-->>Plugin2: rcode, 응답 메시지
        Note over Plugin2: 응답을 캐시에 저장
    end

    Plugin2-->>Plugin1: rcode 반환
    Note over Plugin1: 쿼리 정보 로깅 완료
    Plugin1-->>Server: rcode 반환

    Note over Server: ClientWrite(rcode) 확인
    alt rcode가 SERVFAIL/REFUSED 등
        Server->>ScrubW: errorFunc() → 에러 응답
    end

    ScrubW->>ScrubW: SizeAndDo() + Scrub()
    ScrubW-->>Client: DNS 응답
```

## 2. 플러그인 체인 실행 상세

플러그인 체인의 조립과 실행 메커니즘을 보여준다.

```mermaid
sequenceDiagram
    participant Setup as Setup Phase
    participant NewSrv as NewServer()
    participant PluginList as site.Plugin[]
    participant Stack as stack (Handler)
    participant Chain as pluginChain

    Note over Setup: Corefile 파싱 완료 후

    Setup->>NewSrv: NewServer(addr, group)

    loop 역순 순회 (i = len-1 → 0)
        NewSrv->>PluginList: site.Plugin[i]
        Note over PluginList: Plugin func(Handler) Handler
        PluginList->>Stack: plugin(stack) → 새 Handler
        Note over Stack: 새 Handler.Next = 이전 stack
        NewSrv->>NewSrv: registerHandler(stack)

        alt stack이 MetadataCollector 구현
            NewSrv->>NewSrv: site.metaCollector = stack
        end
        alt stack.Name() == "trace"
            NewSrv->>NewSrv: s.trace = stack
        end
    end

    NewSrv->>Chain: site.pluginChain = stack

    Note over Chain: 실행 시:
    Note over Chain: chain.ServeDNS(ctx, w, r)
    Note over Chain: → plugin.NextOrFailure(name, next, ctx, w, r)
    Note over Chain: → next.ServeDNS(ctx, w, r)
    Note over Chain: → ...재귀적으로 체인 끝까지
```

## 3. Cache 히트/미스 흐름

Cache 플러그인의 캐시 조회, 저장, 프리페치 로직이다.

```mermaid
sequenceDiagram
    participant Req as 요청
    participant Cache as Cache.ServeDNS()
    participant PCache as pcache (양성)
    participant NCache as ncache (음성)
    participant Next as Next Plugin
    participant RW as ResponseWriter

    Req->>Cache: ServeDNS(ctx, w, r)
    Note over Cache: Zone 매칭 확인

    Cache->>Cache: hash(qname, qtype, do, cd)

    Cache->>NCache: ncache.Get(key)
    alt 음성 캐시 히트
        Note over NCache: TTL 확인
        alt TTL > 0
            NCache-->>Cache: 캐시 아이템 반환
            Cache->>RW: item.toMsg() → WriteMsg()
            Cache-->>Req: RcodeSuccess
        else TTL < 0 (만료)
            alt staleUpTo 이내
                Note over Cache: 노화 서빙 (stale serve)
                Cache->>RW: 0 TTL로 응답
            else staleUpTo 초과
                Note over Cache: 캐시 미스 처리
            end
        end
    end

    Cache->>PCache: pcache.Get(key)
    alt 양성 캐시 히트
        PCache-->>Cache: 캐시 아이템 반환
        Note over Cache: TTL 잔여량 확인

        alt shouldPrefetch(item, now)
            Note over Cache: 히트 수 >= prefetch 임계값<br/>AND TTL <= percentage 임계값
            Cache->>Next: go doPrefetch() (별도 고루틴)
        end

        Cache->>RW: item.toMsg() → WriteMsg()
        Cache-->>Req: RcodeSuccess
    end

    Note over Cache: 캐시 미스

    Cache->>Next: doRefresh() → Next.ServeDNS()
    Next-->>Cache: rcode, 응답

    Note over Cache: ResponseWriter가<br/>WriteMsg() 시 캐시 저장

    Cache-->>Req: rcode
```

## 4. Forward 포워딩 흐름

Forward 플러그인이 업스트림 서버에 쿼리를 전달하는 과정이다.

```mermaid
sequenceDiagram
    participant Req as 요청
    participant Fwd as Forward.ServeDNS()
    participant Policy as Policy.List()
    participant Proxy as Proxy
    participant Upstream as 업스트림 DNS
    participant HC as HealthCheck

    Req->>Fwd: ServeDNS(ctx, w, r)

    Note over Fwd: 1. from 도메인 매칭 확인
    alt 매칭 안 됨
        Fwd->>Fwd: NextOrFailure() → 다음 플러그인
        Fwd-->>Req: rcode
    end

    alt maxConcurrent > 0
        Note over Fwd: 동시 쿼리 수 확인
        alt 초과 시
            Fwd-->>Req: RcodeRefused, ErrLimitExceeded
        end
    end

    Fwd->>Policy: f.List() → 프록시 목록 (정책 적용)
    Policy-->>Fwd: [proxy1, proxy2, ...]

    loop deadline(5초) 이내 && 프록시 순회
        Fwd->>Proxy: proxy.Down(maxfails) 확인
        alt 프록시 다운
            Note over Fwd: 다음 프록시 시도
            alt 모든 프록시 다운
                alt failfastUnhealthyUpstreams
                    Fwd-->>Req: RcodeServerFailure
                else
                    Note over Fwd: 랜덤 프록시 선택
                end
            end
        end

        Fwd->>Proxy: proxy.Connect(ctx, state, opts)
        Proxy->>Upstream: DNS 쿼리 전송

        alt 연결 오류 (ErrCachedClosed)
            Note over Proxy: TCP 캐시 연결 닫힘 → 재시도
        end

        alt 응답 Truncated && PreferUDP
            Note over Fwd: ForceTCP로 재시도
        end

        Upstream-->>Proxy: DNS 응답
        Proxy-->>Fwd: ret (*dns.Msg), err

        alt err != nil
            Fwd->>HC: proxy.Healthcheck() 트리거
            Note over Fwd: 다음 프록시 시도
        else 응답 수신 성공
            Note over Fwd: state.Match(ret) 검증

            alt failoverRcodes 매칭
                Note over Fwd: 다음 프록시 시도
            end

            Fwd->>Req: w.WriteMsg(ret)
            Fwd-->>Req: 0, nil
        end
    end

    Fwd-->>Req: RcodeServerFailure
```

## 5. Kubernetes 서비스 조회 흐름

Kubernetes 플러그인이 K8s API 서버 데이터를 기반으로 DNS 응답을 생성하는 과정이다.

```mermaid
sequenceDiagram
    participant Client as DNS 클라이언트
    participant K8s as Kubernetes.ServeDNS()
    participant Parse as parseRequest()
    participant Ctrl as dnsController
    participant Cache as Informer Cache
    participant API as K8s API Server

    Note over Ctrl,API: 사전 준비: Informer가<br/>Service/Endpoint Watch 중

    API->>Ctrl: Watch 이벤트 (Add/Update/Delete)
    Ctrl->>Cache: 로컬 캐시 갱신

    Client->>K8s: my-svc.default.svc.cluster.local A?

    K8s->>K8s: Zone 매칭 확인
    K8s->>Parse: parseRequest(state)
    Note over Parse: QNAME 파싱:<br/>service=my-svc<br/>namespace=default<br/>typeName=svc

    Parse-->>K8s: recordRequest 반환

    K8s->>Ctrl: Services(state, exact)
    Ctrl->>Cache: ServiceList 조회
    Note over Cache: namespace + serviceName<br/>필터링

    Cache-->>Ctrl: Service 객체 반환
    Ctrl-->>K8s: []msg.Service

    alt 서비스 존재
        Note over K8s: QType에 따라 RR 생성
        alt A 레코드
            K8s->>K8s: A{ClusterIP} 생성
        else SRV 레코드
            K8s->>K8s: SRV + A 추가 섹션 생성
        else PTR 레코드 (역방향)
            K8s->>K8s: PTR{serviceName} 생성
        end
        K8s->>Client: DNS 응답 (TTL: 5초 기본)
    else 서비스 없음
        alt Fall-through 설정됨
            K8s->>K8s: NextOrFailure() → 다음 플러그인
        else
            K8s->>Client: NXDOMAIN
        end
    end
```

## 6. Zone 파일 로드 흐름

File 플러그인이 Zone 파일을 로드하고 쿼리에 응답하는 과정이다.

```mermaid
sequenceDiagram
    participant Setup as setup()
    participant File as File 플러그인
    participant ZoneF as Zone 파일
    participant Zone as Zone 구조체
    participant Tree as 레코드 트리
    participant Client as DNS 클라이언트

    Note over Setup: Corefile 파싱 시

    Setup->>File: File 플러그인 초기화
    File->>ZoneF: Zone 파일 읽기 (예: db.example.com)
    ZoneF-->>File: Zone 데이터

    File->>Zone: Zone 구조체 생성
    Note over Zone: SOA 레코드 파싱<br/>Origin 설정

    loop 각 RR 파싱
        File->>Zone: zone.Insert(rr)
        Zone->>Tree: 레코드 트리에 삽입
        Note over Tree: QNAME 기준 B-Tree
    end

    Note over File: Transfer 플러그인 연동<br/>(Zone 전송 지원)

    Client->>File: ServeDNS(ctx, w, r)
    File->>File: Zone 매칭 확인
    File->>Zone: zone.Lookup(state, qname)

    Zone->>Tree: 트리 검색
    alt 정확히 매칭
        Tree-->>Zone: RR 목록
    else 와일드카드 매칭
        Note over Tree: *.example.com 검색
        Tree-->>Zone: 와일드카드 RR
    else CNAME 체인
        Note over Zone: CNAME 따라가기
        Zone->>Tree: CNAME 대상 검색
    end

    Zone-->>File: 응답 메시지 구성
    Note over File: 권한 섹션 (NS)<br/>추가 섹션 (Glue) 추가

    File-->>Client: DNS 응답

    Note over Zone: 주기적 Zone 파일 변경 감시<br/>(reload 플러그인과 연동)
```

## 7. 서버 초기화 흐름 (보너스)

CoreDNS 프로세스 시작부터 서버가 리스닝을 시작하기까지의 전체 초기화 과정이다.

```mermaid
sequenceDiagram
    participant Main as main()
    participant Run as coremain.Run()
    participant Caddy as caddy.Start()
    participant Ctx as dnsContext
    participant Setup as plugin setup()
    participant Srv as Server

    Main->>Main: import _ core/plugin<br/>(zplugin.go: 모든 플러그인 등록)
    Main->>Run: coremain.Run()

    Run->>Run: caddy.TrapSignals()
    Run->>Run: flag.Parse()
    Run->>Run: maxprocs.Set()

    Run->>Run: caddy.LoadCaddyfile("dns")
    Note over Run: Corefile 파일 로드

    Run->>Caddy: caddy.Start(corefile)

    Caddy->>Ctx: newContext() → dnsContext 생성
    Caddy->>Ctx: InspectServerBlocks()
    Note over Ctx: Zone 주소 정규화<br/>Config 생성<br/>firstConfigInBlock 설정

    loop 각 directive (plugin.cfg 순서)
        Caddy->>Setup: plugin.setup(controller)
        Note over Setup: Corefile 디렉티브 파싱<br/>플러그인 인스턴스 생성
        Setup->>Ctx: config.AddPlugin(plugin)
    end

    Caddy->>Ctx: MakeServers()
    Ctx->>Ctx: propagateConfigParams()
    Ctx->>Ctx: groupConfigsByListenAddr()

    loop 각 주소 그룹
        Ctx->>Srv: makeServersForGroup()
        Note over Srv: 프로토콜별 서버 생성<br/>NewServer / NewServerTLS / ...
        Note over Srv: 플러그인 체인 조립<br/>(역순 순회)
    end

    Caddy->>Srv: Server.Listen() + Serve()
    Caddy->>Srv: Server.ListenPacket() + ServePacket()
    Note over Srv: TCP + UDP 동시 리스닝

    Caddy-->>Run: instance 반환
    Run->>Run: instance.Wait() (블로킹)
```
