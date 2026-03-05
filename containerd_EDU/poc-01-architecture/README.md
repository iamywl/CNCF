# PoC-01: gRPC 서버/클라이언트 + 플러그인 등록 아키텍처

## 목적

containerd의 핵심 아키텍처를 시뮬레이션한다:

1. **플러그인 등록 시스템** — `Registration{Type, ID, Requires, InitFn}`으로 플러그인을 정의하고 전역 레지스트리에 등록
2. **Graph() DFS 의존성 정렬** — `Requires` 필드 기반으로 플러그인 초기화 순서를 자동 결정
3. **서버 초기화 흐름** — 정렬된 순서대로 `InitFn` 호출, gRPC/TTRPC 서비스 인터페이스 체크
4. **4개 리스너 병렬 운영** — gRPC, TTRPC, Metrics(Prometheus), Debug(pprof)

## 핵심 개념

### 플러그인 기반 아키텍처

containerd의 모든 기능(Content Store, Snapshotter, Runtime, GC 등)은 플러그인으로 구현된다. 각 플러그인은 `Registration` 구조체로 정의되며, `Type`과 `ID`의 조합(`URI`)으로 고유 식별된다.

```
Registration{
    Type:     "io.containerd.content.v1",     // 플러그인 유형
    ID:       "content",                       // 플러그인 ID
    Requires: []Type{EventPlugin},             // 의존하는 플러그인 타입
    InitFn:   func(ic) (interface{}, error),   // 초기화 함수
}
```

### Graph() 의존성 정렬

`children()` 함수가 재귀적 DFS로 의존성 트리를 순회하여, 피의존 플러그인이 먼저 초기화되도록 보장한다:

```
Event → Content → Snapshot → Metadata → GC
                                      → Runtime → gRPC 서비스들
```

### 4개 리스너

| 리스너 | 주소 | 프로토콜 | 용도 |
|--------|------|----------|------|
| gRPC | `/run/containerd/containerd.sock` | Unix Socket | 클라이언트 API (ctr, crictl) |
| TTRPC | `/run/containerd/containerd.sock.ttrpc` | Unix Socket | Shim 통신 (낮은 오버헤드) |
| Metrics | `0.0.0.0:1338` | TCP/HTTP | Prometheus 메트릭 |
| Debug | `/run/containerd/debug.sock` | Unix Socket | pprof, expvar |

## 실제 소스 참조

| PoC 구현 | 실제 소스 경로 | 설명 |
|----------|---------------|------|
| `Type` 상수 | `plugins/types.go` | `GRPCPlugin`, `ContentPlugin`, `SnapshotPlugin` 등 26종 |
| `Registration` 구조체 | `vendor/github.com/containerd/plugin/plugin.go:59` | Type, ID, Requires, InitFn, ConfigMigration |
| `Graph()` 함수 | `vendor/github.com/containerd/plugin/plugin.go:112` | DisableFilter 적용 후 DFS 정렬 |
| `children()` 함수 | `vendor/github.com/containerd/plugin/plugin.go:135` | 재귀 DFS — Requires 타입 매칭 |
| `Register()` 함수 | `vendor/github.com/containerd/plugin/plugin.go:151` | Type/ID 필수, URI 중복 panic |
| `Server` 구조체 | `cmd/containerd/server/server.go:403` | grpcServer, ttrpcServer, tcpServer, plugins |
| `New()` 함수 | `cmd/containerd/server/server.go:131` | LoadPlugins → Graph → InitFn → 서비스 등록 |
| `LoadPlugins()` | `cmd/containerd/server/server.go:494` | config.ProxyPlugins 처리 + registry.Graph() |
| `ServeGRPC()` | `cmd/containerd/server/server.go:414` | grpcServer.Serve(l) |
| `ServeTTRPC()` | `cmd/containerd/server/server.go:420` | ttrpcServer.Serve(ctx, l) |
| `ServeMetrics()` | `cmd/containerd/server/server.go:425` | HTTP `/v1/metrics` 핸들러 |
| `ServeDebug()` | `cmd/containerd/server/server.go:442` | HTTP `/debug/pprof/*`, `/debug/vars` |
| `Stop()` | `cmd/containerd/server/server.go:460` | grpcServer.Stop() + 플러그인 역순 Close |

## 실행 방법

```bash
cd containerd_EDU/poc-01-architecture
go run main.go
```

## 예상 출력

```
======================================================================
containerd 서버 초기화 시뮬레이션
======================================================================

[1단계] Graph() — DFS 의존성 정렬
  등록된 플러그인 수: 9
  정렬된 초기화 순서:
    1. io.containerd.event.v1.exchange                      (의존: 없음)
    2. io.containerd.content.v1.content                     (의존: io.containerd.event.v1)
    3. io.containerd.snapshotter.v1.overlayfs               (의존: io.containerd.content.v1)
    4. io.containerd.metadata.v1.bolt                       (의존: ...)
    5. io.containerd.gc.v1.scheduler                        (의존: io.containerd.metadata.v1)
    ...

[2단계] 플러그인 순차 초기화 (InitFn 호출)
  Loading: io.containerd.event.v1.exchange                     [OK]
  Loading: io.containerd.content.v1.content                    [OK]
  ...

[3단계] gRPC/TTRPC 서비스 등록
  gRPC 서비스: [containerd.services.content.v1.Content ...]
  TTRPC 서비스: [containerd.services.images.v1.Images ...]

[4단계] 4개 리스너 시작
  [gRPC]    리스닝: 127.0.0.1:xxxxx
  [Metrics] 리스닝: 127.0.0.1:xxxxx/v1/metrics
  [Debug]   리스닝: 127.0.0.1:xxxxx/debug/pprof

[5단계] 클라이언트 요청 시뮬레이션
  --- gRPC 클라이언트 ---
  응답: gRPC response: services=[...]

  --- Metrics 클라이언트 ---
  응답:
    # containerd metrics simulation
    containerd_plugin_count 8
    ...

[6단계] 서버 종료
  플러그인 역순 종료:
    Closing: io.containerd.runtime.v2.task
    Closing: io.containerd.grpc.v1.tasks
    ...

======================================================================
Graph() 필터링 데모: GC 플러그인 비활성화
======================================================================
  비활성화: map[io.containerd.gc.v1.scheduler:true]
  필터링 후 플러그인 수: 8 (원본: 9)
```
