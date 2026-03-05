# PoC-02: ServiceDesc/MethodDesc 등록 및 디스패치

## 개념

gRPC 서버의 서비스 등록 데이터 모델과 메서드 디스패치 메커니즘을 시뮬레이션한다.

```
ServiceDesc (등록 시점)              serviceInfo (런타임)
┌─────────────────────┐             ┌──────────────────────┐
│ ServiceName: "math"  │   변환     │ serviceImpl: &Calc{}   │
│ Methods: [           │ ────────▶  │ methods: map[string]*  │
│   {Add, handler},    │            │   "Add" → &MethodDesc  │
│   {Mul, handler}     │            │   "Mul" → &MethodDesc  │
│ ]                    │            │ streams: map[string]*  │
│ Streams: [...]       │            │   "Sub" → &StreamDesc  │
└─────────────────────┘             └──────────────────────┘
                                        ▲
handleStream("/math.Calculator/Add")    │
  → 서비스명 파싱 → 맵 조회 ─────────────┘
```

## 시뮬레이션하는 gRPC 구조

| 구조체/함수 | 실제 위치 | 역할 |
|------------|----------|------|
| `MethodDesc` | `server.go:99` | Unary RPC 메서드 기술 |
| `StreamDesc` | `server.go:88` | Streaming RPC 메서드 기술 |
| `ServiceDesc` | `server.go:105` | 서비스 전체 기술 (메서드+스트림+메타데이터) |
| `serviceInfo` | `server.go:117` | 내부 관리용 (배열→맵 변환) |
| `RegisterService` | `server.go` | 등록 (ServiceDesc → serviceInfo) |
| `GetServiceInfo` | `server.go` | 외부 조회용 정보 반환 |
| `handleStream` | `server.go` | fullMethod 파싱 → 핸들러 디스패치 |

## 실행 방법

```bash
cd poc-02-data-model
go run main.go
```

## 예상 출력

```
=== ServiceDesc/MethodDesc 등록 및 디스패치 시뮬레이션 ===

── 1. 서비스 등록 ──
[등록] math.Calculator: unary=2, stream=0
[등록] chat.ChatService: unary=1, stream=3

── 2. 등록된 서비스 조회 (GetServiceInfo) ──
서비스: math.Calculator (metadata: math/calculator.proto)
  - Add [Unary]
  - Multiply [Unary]
서비스: chat.ChatService (metadata: chat/service.proto)
  - GetHistory [Unary]
  - Subscribe [Server Streaming]
  - Upload [Client Streaming]
  - LiveChat [Bidi Streaming]

── 3. handleStream 디스패치 ──
[디스패치] fullMethod=/math.Calculator/Add
  서비스=math.Calculator, 메서드=Add
  [Unary] 핸들러 호출
  [응답] {"Result":42}
...

=== 시뮬레이션 완료 ===
```

## 핵심 포인트

1. **배열→맵 변환**: `ServiceDesc`의 `Methods[]`/`Streams[]`를 `serviceInfo`의 `map[string]*`로 변환하여 O(1) 조회
2. **fullMethod 파싱**: `/package.service/method` → 서비스명, 메서드명 분리
3. **Unary vs Stream 분기**: `methods` 맵에서 먼저 찾고, 없으면 `streams` 맵에서 찾기
4. **4가지 스트림 타입**: ServerStreams/ClientStreams 플래그 조합으로 분류
