# 23. RPC API 시스템 심화

## 목차
1. [개요](#1-개요)
2. [아키텍처](#2-아키텍처)
3. [Plugin Server 구조](#3-plugin-server-구조)
4. [Handshake & Magic Cookie](#4-handshake--magic-cookie)
5. [Handle 테이블 시스템](#5-handle-테이블-시스템)
6. [Setup Server & 서비스 초기화](#6-setup-server--서비스-초기화)
7. [CLI Command 진입점](#7-cli-command-진입점)
8. [Stopper 패턴](#8-stopper-패턴)
9. [gRPC 서비스 구조](#9-grpc-서비스-구조)
10. [텔레메트리 통합](#10-텔레메트리-통합)
11. [왜(Why) 이렇게 설계했나](#11-왜why-이렇게-설계했나)
12. [PoC 매핑](#12-poc-매핑)

---

## 1. 개요

Terraform의 **RPC API**는 자동화 도구(특히 HCP Terraform)가 Terraform Core를 프로그래밍 방식으로 제어할 수 있게 하는 gRPC 기반 인터페이스다. CLI 레이어를 우회하여 Plan/Apply/State 작업을 직접 호출할 수 있다.

```
소스 경로:
├── internal/rpcapi/
│   ├── server.go              # ServePlugin, 핸드셰이크 (79줄)
│   ├── cli.go                 # CLICommandFactory, cliCommand (123줄)
│   ├── setup.go               # setupServer, Handshake RPC (76줄)
│   ├── handles.go             # handleTable, 제네릭 핸들 관리 (308줄)
│   ├── stopper.go             # 정지 신호 관리
│   ├── plugin.go              # corePlugin 구현
│   ├── stacks.go              # Stack 관련 RPC
│   ├── packages.go            # 패키지 관련 RPC
│   ├── dependencies.go        # 의존성 관련 RPC
│   ├── telemetry.go           # OpenTelemetry 통합
│   ├── convert.go             # protobuf ↔ 내부 타입 변환
│   ├── terraform1/            # protobuf 정의
│   └── dynrpcserver/          # 동적 서비스 등록
```

### 핵심 설계 특징

| 특징 | 구현 |
|------|------|
| go-plugin 프레임워크 | HashiCorp의 플러그인 프레임워크 위에 구축 |
| gRPC over stdio | 부모-자식 프로세스 간 gRPC 통신 |
| 핸드셰이크 프로토콜 | Magic Cookie로 실행 환경 검증 |
| Handle 기반 리소스 관리 | 정수 핸들로 서버 측 객체 참조 |
| 서비스 지연 초기화 | Handshake 이후 능력 협상에 따라 서비스 활성화 |

---

## 2. 아키텍처

### 전체 시스템 흐름

```
┌──────────────────────┐         ┌──────────────────────┐
│   HCP Terraform      │         │   terraform rpcapi   │
│   (Plugin Client)     │         │   (Plugin Server)    │
│                       │         │                      │
│  go-plugin client     │  stdio  │  go-plugin server    │
│  ┌─────────────────┐ │◄──────►│ ┌──────────────────┐ │
│  │ gRPC Client     │ │ gRPC   │ │ gRPC Server      │ │
│  │                 │ │        │ │                  │ │
│  │ Setup.Handshake │─┤────────├─│ setupServer      │ │
│  │ Setup.Stop      │ │        │ │                  │ │
│  │ Stacks.*        │ │        │ │ stacksServer     │ │
│  │ Packages.*      │ │        │ │ packagesServer   │ │
│  │ Dependencies.*  │ │        │ │ dependenciesServer│ │
│  └─────────────────┘ │        │ └──────────────────┘ │
└──────────────────────┘         └──────────────────────┘
```

### 프로세스 생명주기

```
1. HCP Terraform가 `terraform rpcapi` 프로세스 시작
   └── 환경변수 TERRAFORM_RPCAPI_COOKIE 설정

2. terraform rpcapi → ServePlugin() 호출
   ├── Magic Cookie 확인
   └── go-plugin.Serve() 호출 → gRPC 서버 시작

3. 클라이언트 → Setup.Handshake() 호출
   ├── 능력 협상 (capabilities)
   └── 다른 서비스 초기화 (Stacks, Packages, Dependencies)

4. 클라이언트 → 업무 RPC 호출
   ├── 소스 번들 열기 → handle 반환
   ├── 스택 설정 로드 → handle 반환
   ├── Plan 실행 → 결과 반환
   └── ...

5. 클라이언트 → Setup.Stop() 호출 또는 Context 취소
   └── GracefulStop() → 프로세스 종료
```

---

## 3. Plugin Server 구조

### `internal/rpcapi/server.go`

```go
func ServePlugin(ctx context.Context, opts ServerOpts) error {
    // 사전 검증: Magic Cookie가 없으면 플러그인 클라이언트가 아님
    if os.Getenv(handshake.MagicCookieKey) != handshake.MagicCookieValue {
        return ErrNotPluginClient
    }

    plugin.Serve(&plugin.ServeConfig{
        HandshakeConfig: handshake,
        VersionedPlugins: map[int]plugin.PluginSet{
            1: {
                "tfcore": &corePlugin{
                    experimentsAllowed: opts.ExperimentsAllowed,
                },
            },
        },
        GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
            fullOpts := []grpc.ServerOption{
                grpc.UnaryInterceptor(otelgrpc.UnaryServerInterceptor()),
                grpc.StreamInterceptor(otelgrpc.StreamServerInterceptor()),
            }
            fullOpts = append(fullOpts, opts...)
            server := grpc.NewServer(fullOpts...)

            // Context 취소 시 Graceful Shutdown
            go func() {
                <-ctx.Done()
                server.GracefulStop()
                os.Exit(0)
            }()
            return server
        },
    })
    return nil
}
```

### Handshake 설정

```go
var handshake = plugin.HandshakeConfig{
    ProtocolVersion:  1,
    MagicCookieKey:   "TERRAFORM_RPCAPI_COOKIE",
    MagicCookieValue: "fba0991c9bcd453982f0d88e2da95940",
}
```

---

## 4. Handshake & Magic Cookie

### Magic Cookie 메커니즘

```
┌─────────────────────────────┐
│ 부모 프로세스 (HCP Terraform)│
│                              │
│ env: TERRAFORM_RPCAPI_COOKIE │
│    = fba0991c...             │
│                              │
│ exec("terraform", "rpcapi")  │
└──────────────┬───────────────┘
               │ 자식 프로세스 시작
               ▼
┌─────────────────────────────┐
│ 자식 프로세스 (terraform)    │
│                              │
│ os.Getenv("TERRAFORM_RPCAPI │
│ _COOKIE") == "fba0991c..."?  │
│   ├── Yes → 플러그인 모드    │
│   └── No  → ErrNotPluginClient│
└──────────────────────────────┘
```

**왜 Magic Cookie인가?**

사용자가 `terraform rpcapi`를 직접 실행하는 것을 방지한다. 이 명령은 HCP Terraform의 자동화 파이프라인에서만 사용되도록 설계되었으며, 직접 실행 시 명확한 에러 메시지를 표시한다:

> This subcommand is for use by HCP Terraform and is not intended for direct use.

---

## 5. Handle 테이블 시스템

### 핵심 설계

`internal/rpcapi/handles.go`는 Go 제네릭을 활용한 타입 안전 핸들 시스템이다.

```go
// 핸들 = 정수 식별자
type handle[T any] int64

// 핸들 테이블 = 공유 객체 저장소
type handleTable struct {
    handleObjs map[int64]any        // 핸들 → 객체 매핑
    nextHandle int64                 // 다음 할당 핸들 번호
    handleDeps map[int64]map[int64]struct{}  // 의존성 추적
    mu         sync.Mutex            // 동시성 보호
}
```

### 핸들 연산

```
┌─────────────────────────────────────────────┐
│ handleTable                                  │
│                                              │
│ handleObjs:                                  │
│   1 → *sourcebundle.Bundle                  │
│   2 → *stackconfig.Config   ──depends──> 1   │
│   3 → *stackstate.State                      │
│   4 → *stackplan.Plan                        │
│                                              │
│ handleDeps:                                  │
│   1 → {2}  (1번을 닫으려면 2번을 먼저 닫아야)│
│                                              │
│ nextHandle: 5                                │
└─────────────────────────────────────────────┘
```

### 핸들 생성

```go
func newHandle[ObjT any](t *handleTable, obj ObjT) handle[ObjT] {
    t.mu.Lock()
    hnd := t.nextHandle
    t.nextHandle++
    t.handleObjs[hnd] = obj
    t.mu.Unlock()
    return handle[ObjT](hnd)
}
```

### 의존성이 있는 핸들 생성

```go
func newHandleWithDependency[ObjT, DepT any](t *handleTable, obj ObjT, dep handle[DepT]) (handle[ObjT], error) {
    t.mu.Lock()
    // 의존 대상이 존재하는지 확인
    if _, exists := t.handleObjs[int64(dep)]; !exists {
        return handle[ObjT](0), newHandleErrorNoParent
    }
    hnd := t.nextHandle
    t.nextHandle++
    t.handleObjs[hnd] = obj

    // 의존성 기록
    if _, exists := t.handleDeps[int64(dep)]; !exists {
        t.handleDeps[int64(dep)] = make(map[int64]struct{})
    }
    t.handleDeps[int64(dep)][hnd] = struct{}{}
    t.mu.Unlock()
    return handle[ObjT](hnd), nil
}
```

### 핸들 닫기 (의존성 검사 포함)

```go
func closeHandle[ObjT any](t *handleTable, hnd handle[ObjT]) error {
    t.mu.Lock()
    defer t.mu.Unlock()

    // 존재 확인
    if _, exists := t.handleObjs[int64(hnd)]; !exists {
        return closeHandleErrorUnknown
    }

    // 다른 핸들이 이 핸들에 의존하는지 확인
    if len(t.handleDeps[int64(hnd)]) > 0 {
        return closeHandleErrorBlocked  // 의존 핸들이 있으면 닫기 거부
    }

    delete(t.handleObjs, int64(hnd))

    // 이 핸들의 의존성 정리
    for _, m := range t.handleDeps {
        delete(m, int64(hnd))
    }
    return nil
}
```

### 왜 단일 숫자 공간인가?

소스 코드 주석에 명시:

> In our public API contract each different object has a separate numberspace
> of handles, but as an implementation detail we share a single numberspace
> for everything just because that means that if a client gets their handles
> mixed up then it'll fail with an error rather than potentially doing
> something unexpected to an unrelated object.

클라이언트가 소스 번들 핸들을 스택 설정 핸들 자리에 넣으면, 타입 불일치로 즉시 에러가 발생한다. 별도 숫자 공간이었다면 우연히 같은 번호의 다른 객체를 조작할 위험이 있다.

---

## 6. Setup Server & 서비스 초기화

### `internal/rpcapi/setup.go`

```go
type setupServer struct {
    setup.UnimplementedSetupServer
    initOthers func(context.Context, *setup.Handshake_Request, *stopper) (*setup.ServerCapabilities, error)
    stopper    *stopper
    mu         sync.Mutex
}
```

### Handshake RPC

```go
func (s *setupServer) Handshake(ctx context.Context, req *setup.Handshake_Request) (*setup.Handshake_Response, error) {
    s.mu.Lock()
    defer s.mu.Unlock()

    if s.initOthers == nil {
        // 이미 핸드셰이크 완료 → 재시도 거부
        return nil, status.Error(codes.FailedPrecondition, "handshake already completed")
    }

    serverCaps, err := s.initOthers(ctx, req, s.stopper)
    s.initOthers = nil  // 다시 핸드셰이크할 수 없음
    // ...
}
```

**왜 핸드셰이크를 한 번만 허용하는가?**

서비스 초기화에는 부작용(side effects)이 있다. 핸들 테이블 생성, gRPC 서비스 등록 등이 한 번만 수행되어야 하며, 반복 실행하면 중복 등록이나 상태 불일치가 발생한다. `initOthers`를 nil로 설정하여 물리적으로 재실행을 방지한다.

### Stop RPC

```go
func (s *setupServer) Stop(ctx context.Context, req *setup.Stop_Request) (*setup.Stop_Response, error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.stopper.stop()  // 모든 실행 중인 작업에 정지 신호 전파
    return &setup.Stop_Response{}, nil
}
```

---

## 7. CLI Command 진입점

### `internal/rpcapi/cli.go`

```go
func CLICommandFactory(opts CommandFactoryOpts) func() (cli.Command, error) {
    return func() (cli.Command, error) {
        return cliCommand{opts}, nil
    }
}

func (c cliCommand) Run(args []string) int {
    if len(args) != 0 {
        return 1  // 인자 불허
    }

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // Shutdown 채널 → Context 취소로 변환
    go func() {
        select {
        case <-c.opts.ShutdownCh:
            cancel()
        case <-ctx.Done():
            return
        }
    }()

    err := ServePlugin(ctx, ServerOpts{
        ExperimentsAllowed: c.opts.ExperimentsAllowed,
    })
    // ...
}
```

### 실행 흐름

```
terraform rpcapi
    │
    ├── CLICommandFactory → cliCommand 생성
    │
    ├── cliCommand.Run()
    │   ├── Context 생성
    │   ├── Shutdown 채널 모니터링 고루틴 시작
    │   └── ServePlugin() 호출 (블로킹)
    │
    ├── 정상 케이스: ServePlugin이 영원히 블로킹 → os.Exit(0)
    │
    └── 비정상 케이스: ErrNotPluginClient → 에러 메시지 출력
```

---

## 8. Stopper 패턴

Stopper는 실행 중인 장기 작업(Plan, Apply)을 정상적으로 중단하는 협력적 취소 메커니즘이다.

### 동작 원리

```
클라이언트 → Stop RPC 호출
    │
    ▼
stopper.stop()
    │
    ▼
모든 실행 중인 작업이 stopper 신호를 확인
    │
    ▼
작업들이 현재 단계 완료 후 정상 종료
```

이는 Go의 `context.Context` 취소 패턴과 유사하지만, gRPC 레벨에서 작동한다. 클라이언트가 개별 RPC 호출의 context를 취소하는 것이 아니라, 서버 전체의 작업을 중단시키는 글로벌 정지 신호다.

---

## 9. gRPC 서비스 구조

### 지원되는 리소스 타입과 핸들

```go
// 소스 번들
func (t *handleTable) NewSourceBundle(sources *sourcebundle.Bundle) handle[*sourcebundle.Bundle]
func (t *handleTable) CloseSourceBundle(hnd handle[*sourcebundle.Bundle]) error

// 스택 설정 (소스 번들에 의존)
func (t *handleTable) NewStackConfig(cfg *stackconfig.Config,
    owningSourceBundle handle[*sourcebundle.Bundle]) (handle[*stackconfig.Config], error)

// 스택 상태
func (t *handleTable) NewStackState(state *stackstate.State) handle[*stackstate.State]

// 스택 Plan
func (t *handleTable) NewStackPlan(state *stackplan.Plan) handle[*stackplan.Plan]

// Terraform 상태
func (t *handleTable) NewTerraformState(state *states.State) handle[*states.State]

// 의존성 잠금
func (t *handleTable) NewDependencyLocks(locks *depsfile.Locks) handle[*depsfile.Locks]

// Provider 플러그인 캐시
func (t *handleTable) NewProviderPluginCache(dir *providercache.Dir) handle[*providercache.Dir]

// 스택 인스펙터
func (t *handleTable) NewStackInspector(dir *stacksInspector) handle[*stacksInspector]
```

### 의존성 그래프

```
sourcebundle.Bundle ◄───── stackconfig.Config
                              (소스 번들 닫으면 설정 무효화)

stackstate.State     (독립)
stackplan.Plan       (독립)
states.State         (독립)
depsfile.Locks       (독립 - 로드 후 메모리에 독립)
providercache.Dir    (독립)
stacksInspector      (독립)
```

DependencyLocks이 SourceBundle에 의존하지 않는 이유는 코드 주석에 명시:

> Not all lock objects necessarily originate from lock files. [...] The locks
> object in memory is not dependent on the lock file it was loaded from once
> the load is complete.

---

## 10. 텔레메트리 통합

### OpenTelemetry 인터셉터

```go
GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
    fullOpts := []grpc.ServerOption{
        grpc.UnaryInterceptor(otelgrpc.UnaryServerInterceptor()),
        grpc.StreamInterceptor(otelgrpc.StreamServerInterceptor()),
    }
    fullOpts = append(fullOpts, opts...)
    server := grpc.NewServer(fullOpts...)
    return server
}
```

모든 gRPC 호출에 자동으로 OpenTelemetry 트레이싱 span이 생성된다. 이를 통해:
- 각 RPC 호출의 지연 시간 측정
- 에러 발생 위치 추적
- 분산 트레이싱에서 Terraform Core 호출 시각화

---

## 11. 왜(Why) 이렇게 설계했나

### Q1: 왜 REST가 아닌 gRPC인가?

| 측면 | gRPC | REST |
|------|------|------|
| 타입 안전성 | protobuf 스키마로 보장 | OpenAPI로 수동 관리 |
| 양방향 스트리밍 | 네이티브 지원 | WebSocket 별도 구현 필요 |
| 성능 | 바이너리 프로토콜, HTTP/2 | JSON 텍스트, HTTP/1.1 |
| 코드 생성 | protoc으로 자동 생성 | 수동 또는 codegen 도구 |

Plan/Apply는 장시간 실행되는 작업이며, 실시간 진행 상황 스트리밍이 필요하다. gRPC의 server-side streaming이 이를 자연스럽게 지원한다.

### Q2: 왜 go-plugin 프레임워크를 사용하는가?

HashiCorp의 go-plugin은 이미 Provider/Provisioner 플러그인에서 검증된 프레임워크다:
- stdio 기반 gRPC로 네트워크 포트 충돌 없음
- 부모 프로세스가 자식을 직접 제어 (SIGTERM 등)
- 버전 협상 내장
- 보안: 외부 네트워크에 노출되지 않음

### Q3: 왜 Handle을 닫을 때 전체 스캔을 하는가?

```go
// handleDeps를 전체 스캔하여 닫히는 핸들의 의존성 제거
for _, m := range t.handleDeps {
    delete(m, int64(hnd))
}
```

코드 주석에 이유가 있다:

> Our dependency-tracking data structure is not optimized for deleting because
> that's rare in comparison to adding and checking, but we expect the handle
> table to typically be small enough for this full scan not to hurt.

핸들 닫기는 드문 연산이고, 핸들 테이블 크기가 일반적으로 작으므로(수십 개 수준), 역 인덱스를 유지하는 복잡성보다 단순 스캔이 합리적이다.

### Q4: 왜 실험 기능 플래그가 있는가?

```go
type ServerOpts struct {
    ExperimentsAllowed bool
}
```

RPC API 자체가 실험적 단계에 있으며, 일부 기능은 추가적인 실험 플래그가 필요하다. 이 플래그는 HCP Terraform의 배포 설정에서 제어되며, 프로덕션 환경에서 불안정한 기능의 노출을 방지한다.

---

## 12. PoC 매핑

| PoC | 시뮬레이션 대상 |
|-----|---------------|
| poc-23-rpcapi | Handle 테이블, 의존성 추적, Handshake 프로토콜, Stopper 패턴 |

---

## 참조 소스 파일

| 파일 | 줄수 | 핵심 내용 |
|------|------|----------|
| `internal/rpcapi/server.go` | 79 | ServePlugin, handshake, Magic Cookie |
| `internal/rpcapi/cli.go` | 123 | CLICommandFactory, cliCommand, 실행 흐름 |
| `internal/rpcapi/handles.go` | 308 | handle, handleTable, 의존성 관리 |
| `internal/rpcapi/setup.go` | 76 | setupServer, Handshake/Stop RPC |
