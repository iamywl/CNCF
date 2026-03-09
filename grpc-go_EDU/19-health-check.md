# gRPC-Go Health Check 심화 분석

## 1. 개요

gRPC Health Checking Protocol은 서버의 서비스별 가용 상태를 클라이언트에게 알리기 위한 **표준 프로토콜**이다. Kubernetes liveness/readiness probe, 로드 밸런서 헬스체크, 서비스 메시의 엔드포인트 관리 등에서 핵심적으로 사용된다.

### 왜 별도 Health Check가 필요한가?

TCP 연결이 살아있다고 서비스가 정상인 것은 아니다:

```
┌─────────────────────────────────────────────────────────┐
│  TCP 연결 상태          서비스 상태                       │
│  ────────────          ──────────                       │
│  Connected    ──→      DB 커넥션 풀 고갈                  │
│  Connected    ──→      의존 서비스 장애 전파               │
│  Connected    ──→      메모리 부족으로 OOM 직전            │
│  Connected    ──→      설정 파일 오류로 비즈니스 로직 실패    │
│  Connected    ──→      배포 중 (아직 초기화 미완료)         │
└─────────────────────────────────────────────────────────┘
```

gRPC Health Check는 **애플리케이션 레벨**에서 서비스 상태를 판별하여, 단순 연결 확인으로는 감지할 수 없는 장애를 포착한다.

### 소스코드 위치

```
grpc-go/
├── health/
│   ├── server.go          ← 서버 측 Health Check 서비스 구현 (188줄)
│   ├── client.go          ← 클라이언트 측 Health Check 로직 (118줄)
│   ├── producer.go        ← 클라이언트 연결의 Health Check Producer (107줄)
│   ├── logging.go         ← 로깅 설정
│   └── grpc_health_v1/
│       ├── health.pb.go       ← Protobuf 메시지 정의 (자동 생성)
│       └── health_grpc.pb.go  ← gRPC 서비스 인터페이스 (자동 생성)
└── internal/
    └── internal.go        ← HealthChecker 함수 타입 정의
```

---

## 2. 아키텍처

### 전체 구조

```
┌──────────────────────────────────────────────────────────────────────┐
│                        gRPC Health Check 아키텍처                     │
│                                                                      │
│  ┌─────────── 서버 ───────────┐     ┌──────── 클라이언트 ────────┐    │
│  │                            │     │                            │    │
│  │  ┌─────────────────────┐   │     │   ┌─────────────────────┐  │    │
│  │  │  Application Logic  │   │     │   │     Balancer         │  │    │
│  │  │  (비즈니스 로직)      │   │     │   │  (로드 밸런서)       │  │    │
│  │  └────────┬────────────┘   │     │   └──────────┬──────────┘  │    │
│  │           │                │     │              │             │    │
│  │    SetServingStatus()      │     │    SubConn 상태 갱신       │    │
│  │           │                │     │              │             │    │
│  │  ┌────────▼────────────┐   │     │   ┌──────────▼──────────┐  │    │
│  │  │  Health Server      │◄──┼─────┼──►│  Health Producer     │  │    │
│  │  │  (statusMap)        │   │ gRPC│   │  (clientHealthCheck) │  │    │
│  │  │                     │   │Watch│   │                      │  │    │
│  │  │  Check() ← Unary    │   │     │   │  newStream()         │  │    │
│  │  │  Watch() ← Stream   │   │     │   │  setConnectivityState│  │    │
│  │  └─────────────────────┘   │     │   └──────────────────────┘  │    │
│  └────────────────────────────┘     └────────────────────────────┘    │
└──────────────────────────────────────────────────────────────────────┘
```

### 두 가지 RPC 메서드

| 메서드 | 타입 | 용도 | 장점 |
|--------|------|------|------|
| `Check` | Unary RPC | 일회성 상태 조회 | 간단, 폴링 기반 |
| `Watch` | Server Streaming RPC | 상태 변화 구독 | 효율적, 이벤트 기반 |

### Protobuf 정의

```protobuf
service Health {
  // Check: 즉시 현재 상태 반환
  rpc Check(HealthCheckRequest) returns (HealthCheckResponse);

  // Watch: 상태 변화 시 스트림으로 전송
  rpc Watch(HealthCheckRequest) returns (stream HealthCheckResponse);

  // List: 모든 서비스 상태 스냅샷
  rpc List(HealthListRequest) returns (HealthListResponse);
}

message HealthCheckRequest {
  string service = 1;  // 빈 문자열 = 전체 서버 상태
}

message HealthCheckResponse {
  enum ServingStatus {
    UNKNOWN         = 0;
    SERVING         = 1;
    NOT_SERVING     = 2;
    SERVICE_UNKNOWN = 3;  // Watch 전용
  }
  ServingStatus status = 1;
}
```

---

## 3. 핵심 데이터 구조

### Server 구조체

`health/server.go:41-50`

```go
type Server struct {
    healthgrpc.UnimplementedHealthServer
    mu        sync.RWMutex
    shutdown  bool
    statusMap map[string]healthpb.HealthCheckResponse_ServingStatus
    updates   map[string]map[healthgrpc.Health_WatchServer]chan healthpb.HealthCheckResponse_ServingStatus
}
```

| 필드 | 타입 | 역할 |
|------|------|------|
| `mu` | `sync.RWMutex` | 동시성 제어. Check는 RLock, SetServingStatus/Watch는 Lock |
| `shutdown` | `bool` | 서버 종료 플래그. true이면 모든 상태 변경 무시 |
| `statusMap` | `map[string]ServingStatus` | 서비스명 → 상태 매핑. `""` = 전체 서버 |
| `updates` | 중첩 맵 | `[서비스명][Watch클라이언트] → 상태변화채널` |

### updates 중첩 맵 구조

```
updates = {
  "" (전체 서버): {
    watchClient_A → chan ServingStatus (버퍼=1)
    watchClient_B → chan ServingStatus (버퍼=1)
  },
  "user-service": {
    watchClient_C → chan ServingStatus (버퍼=1)
  },
  "order-service": {
    watchClient_D → chan ServingStatus (버퍼=1)
  }
}
```

**왜 버퍼=1인가?**

```
┌─────────────────────────────────────────────────┐
│  버퍼 크기별 비교                                 │
│                                                  │
│  버퍼 0: 수신자 없으면 전송 블로킹                  │
│    → SetServingStatus()가 Watch 클라이언트 때문에   │
│      블로킹될 위험                                │
│                                                  │
│  버퍼 1: 최신 상태 1개 대기 가능                    │
│    → 클라이언트가 느려도 서버 블로킹 없음            │
│    → select로 이전 상태 폐기 후 최신 상태 채움       │
│                                                  │
│  버퍼 N: 과거 상태 누적 (불필요한 메모리 사용)       │
│    → 상태는 "이벤트 로그"가 아니라 "현재 값"         │
│    → 최신 값만 의미 있음                           │
└─────────────────────────────────────────────────┘
```

### 상태 열거형

`health/grpc_health_v1/health.pb.go:41-48`

| 상태 | 값 | 의미 | 사용 컨텍스트 |
|------|-----|------|-------------|
| `UNKNOWN` | 0 | 초기/미정의 | 서버 시작 전 |
| `SERVING` | 1 | 정상 동작 | 트래픽 수신 가능 |
| `NOT_SERVING` | 2 | 서비스 중단 | 서버 종료, 장애 |
| `SERVICE_UNKNOWN` | 3 | 서비스 미등록 | Watch 전용 (스트림 유지) |

---

## 4. 서버 측 구현 심화

### NewServer — 초기화

`health/server.go:52-58`

```go
func NewServer() *Server {
    return &Server{
        statusMap: map[string]healthpb.HealthCheckResponse_ServingStatus{
            "": healthpb.HealthCheckResponse_SERVING,  // 기본: 전체 서버 SERVING
        },
        updates: make(map[string]map[healthgrpc.Health_WatchServer]chan healthpb.HealthCheckResponse_ServingStatus),
    }
}
```

**왜 빈 문자열("")을 SERVING으로 초기화하는가?**

- 빈 문자열은 전체 서버의 건강 상태를 나타내는 **관례**
- 서버가 시작되면 기본적으로 "건강"으로 간주
- 개별 서비스 등록 전에도 서버 레벨 헬스체크가 동작

### Check — 동기식 상태 조회

`health/server.go:60-70`

```go
func (s *Server) Check(_ context.Context, in *HealthCheckRequest) (*HealthCheckResponse, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    if servingStatus, ok := s.statusMap[in.Service]; ok {
        return &healthpb.HealthCheckResponse{
            Status: servingStatus,
        }, nil
    }
    return nil, status.Error(codes.NotFound, "unknown service")
}
```

**동작 흐름:**

```
Check("user-service")
  │
  ├─ statusMap["user-service"] 존재?
  │   ├─ Yes → {Status: SERVING} 반환
  │   └─ No  → NotFound 에러 반환
```

**설계 결정:**
- `RLock` 사용 — 동시 읽기 허용으로 Check 호출이 서로를 블로킹하지 않음
- 미등록 서비스에 대해 **에러 반환** (Watch와 다른 전략)

### Watch — 스트리밍 상태 구독

`health/server.go:89-132`

```
Watch("user-service") 시작
  │
  ├─ [1] 초기 상태 결정
  │   ├─ statusMap에 존재 → 해당 상태
  │   └─ statusMap에 미존재 → SERVICE_UNKNOWN (스트림 유지!)
  │
  ├─ [2] 구독 등록
  │   └─ updates["user-service"][thisStream] = chan (버퍼=1)
  │
  ├─ [3] 이벤트 루프 (무한)
  │   ├─ case <-update:
  │   │   ├─ 이전과 동일 상태 → 무시 (중복 제거)
  │   │   └─ 새로운 상태 → stream.Send() → 클라이언트에 전송
  │   └─ case <-stream.Context().Done():
  │       └─ 클라이언트 연결 끊김 → 정리 후 종료
  │
  └─ [defer] 구독 해제
      └─ delete(updates["user-service"], thisStream)
```

**왜 Watch에서는 SERVICE_UNKNOWN을 반환하는가? (Check는 NotFound 에러)**

```
시나리오: 서비스가 시작 중

  t=0    클라이언트 Watch("new-service") 시작
         → SERVICE_UNKNOWN 반환 (스트림 유지)

  t=5s   서버: SetServingStatus("new-service", SERVING)
         → Watch 클라이언트에 SERVING 전달

  t=10s  클라이언트: SERVING 수신 → 정상 동작 시작

만약 Check처럼 NotFound 에러를 반환했다면?
  → 스트림 종료 → 클라이언트가 재연결 필요
  → 불필요한 오버헤드, 레이스 컨디션 발생 가능
```

### SetServingStatus — 상태 변경 + 구독자 알림

`health/server.go:136-159`

```go
func (s *Server) SetServingStatus(service string, servingStatus healthpb.HealthCheckResponse_ServingStatus) {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.shutdown {
        return  // 종료 상태에서는 상태 변경 무시
    }
    s.setServingStatusLocked(service, servingStatus)
}

func (s *Server) setServingStatusLocked(service string, servingStatus healthpb.HealthCheckResponse_ServingStatus) {
    s.statusMap[service] = servingStatus
    for _, update := range s.updates[service] {
        // 이전 미소비 상태 폐기 (논블로킹)
        select {
        case <-update:
        default:
        }
        // 최신 상태 전송
        update <- servingStatus
    }
}
```

**비차단 상태 전파 패턴:**

```
┌───────────────────────────────────────────────┐
│  select {                                      │
│  case <-update:  // 이전 상태 버림 (있다면)      │
│  default:        // 채널 비어있으면 skip          │
│  }                                             │
│  update <- servingStatus  // 최신 상태 채움      │
│                                                │
│  이 패턴이 보장하는 것:                           │
│  1. 서버가 절대 블로킹되지 않음                    │
│  2. 채널에는 항상 최신 상태만 존재                  │
│  3. 클라이언트가 느려도 서버 성능에 영향 없음        │
└───────────────────────────────────────────────┘
```

### Shutdown / Resume — 그레이스풀 생명주기

`health/server.go:166-187`

```
Shutdown() 호출
  │
  ├─ shutdown = true
  │   → 이후 SetServingStatus() 호출 무시
  │
  └─ 모든 서비스 → NOT_SERVING
      → 모든 Watch 클라이언트에 NOT_SERVING 전파
      → 로드 밸런서: 이 서버에서 트래픽 빼기


Resume() 호출
  │
  ├─ shutdown = false
  │   → SetServingStatus() 다시 허용
  │
  └─ 모든 서비스 → SERVING
      → 모든 Watch 클라이언트에 SERVING 전파
      → 로드 밸런서: 이 서버로 트래픽 복귀
```

**왜 Shutdown 후 상태 변경을 막는가?**

서버 종료 중에 애플리케이션이 실수로 `SetServingStatus("service", SERVING)`을 호출하면, 종료 중인 서버로 트래픽이 다시 들어올 수 있다. `shutdown` 플래그가 이를 방지한다.

---

## 5. 클라이언트 측 구현 심화

### clientHealthCheck — 클라이언트 헬스체크 루프

`health/client.go:59-117`

```
clientHealthCheck() 시작
  │
  ├─ [재시도 루프]
  │   │
  │   ├─ 백오프 대기 (tryCnt > 0일 때)
  │   │   └─ 지수 백오프: 1s, 2s, 3s, 4s, 5s (최대 5s)
  │   │
  │   ├─ setConnectivityState(Connecting)
  │   │
  │   ├─ newStream("/grpc.health.v1.Health/Watch")
  │   │   ├─ 실패 → continue retryConnection
  │   │   └─ 성공 → 요청 전송
  │   │
  │   ├─ SendMsg(HealthCheckRequest{Service: ""})
  │   │   └─ CloseSend()
  │   │
  │   └─ [수신 루프]
  │       │
  │       ├─ RecvMsg(resp)
  │       │
  │       ├─ Unimplemented 에러?
  │       │   └─ setConnectivityState(Ready)
  │       │      → 서버가 미지원이면 "건강"으로 간주
  │       │
  │       ├─ 기타 에러?
  │       │   └─ setConnectivityState(TransientFailure)
  │       │      → continue retryConnection
  │       │
  │       ├─ resp.Status == SERVING?
  │       │   └─ setConnectivityState(Ready)
  │       │      tryCnt = 0 (백오프 리셋)
  │       │
  │       └─ resp.Status != SERVING?
  │           └─ setConnectivityState(TransientFailure)
  │              tryCnt = 0 (백오프 리셋)
```

### 왜 Unimplemented를 Ready로 처리하는가?

```
시나리오: 레거시 서버 (Health Check 미구현)

  클라이언트: Watch RPC 호출
  서버: Unimplemented 에러 반환

  옵션 A: TransientFailure → 서버 사용 불가 ❌
    → 정상 동작하는 서버를 사용할 수 없음
    → 하위 호환성 파괴

  옵션 B: Ready → 서버 사용 가능 ✅ (실제 구현)
    → "Health Check가 없으면 연결 자체가 건강의 증거"
    → 하위 호환성 유지
    → connection-level health → RPC-level health 폴백
```

### Producer — 로드 밸런서 통합

`health/producer.go:64-106`

```
┌──────────────────────────────────────────────────────────┐
│  로드 밸런서 ↔ Health Producer 통합 흐름                    │
│                                                          │
│  Balancer                                                │
│    │                                                     │
│    ├─ registerClientSideHealthCheckListener()             │
│    │   ├─ SubConn.GetOrBuildProducer()                   │
│    │   │   → 기존 producer 재사용 또는 새로 생성            │
│    │   ├─ p.cancel()  // 이전 헬스체크 취소                │
│    │   └─ go p.startHealthCheck()  // 새 헬스체크 시작     │
│    │                                                     │
│    └─ listener(SubConnState) 콜백                         │
│        ├─ Ready → 이 SubConn으로 트래픽 라우팅             │
│        └─ TransientFailure → 이 SubConn 사용 중단         │
│                                                          │
│  startHealthCheck():                                     │
│    ├─ newStream → Watch RPC 스트림 생성                    │
│    ├─ setConnectivityState → listener 콜백 호출            │
│    └─ internal.HealthCheckFunc()  // clientHealthCheck    │
└──────────────────────────────────────────────────────────┘
```

**init() 등록 메커니즘:**

```go
// health/client.go:51-53
func init() {
    internal.HealthCheckFunc = clientHealthCheck
}
```

`health` 패키지를 import하면 `clientHealthCheck`가 전역 변수에 등록된다. 이 패턴을 사용하는 이유:
- **의존성 분리**: core gRPC 패키지가 health 패키지에 직접 의존하지 않음
- **선택적 기능**: health 패키지를 import하지 않으면 헬스체크 비활성화
- **테스트 용이성**: 테스트에서 mock 함수로 교체 가능

---

## 6. 상태 전이 다이어그램

### 서버 측 상태 전이

```
                    ┌─────────────┐
                    │  NewServer  │
                    └──────┬──────┘
                           │
                    statusMap[""] = SERVING
                           │
                    ┌──────▼──────┐
            ┌──────│   SERVING   │◄─────────────────┐
            │      └──────┬──────┘                  │
            │             │                         │
      SetServingStatus    │  SetServingStatus   Resume()
      (NOT_SERVING)       │  (SERVING)             │
            │             │                         │
            │      ┌──────▼──────┐                  │
            └─────►│ NOT_SERVING │──────────────────┘
                   └──────┬──────┘
                          │
                    Shutdown()
                          │
                   ┌──────▼──────┐
                   │  SHUTDOWN   │  (shutdown=true)
                   │ NOT_SERVING │  (상태 변경 불가)
                   └─────────────┘
```

### 클라이언트 측 연결 상태 전이

```
┌────────────┐     스트림 생성 시작      ┌────────────┐
│    Idle     │ ──────────────────────► │ Connecting  │
└────────────┘                         └──────┬──────┘
                                              │
                              ┌───────────────┼───────────────┐
                              │               │               │
                         스트림 실패     SERVING 수신    NOT_SERVING/에러
                              │               │               │
                       ┌──────▼──────┐ ┌──────▼──────┐ ┌──────▼──────────┐
                       │ Transient   │ │    Ready    │ │   Transient     │
                       │  Failure    │ │  (정상 동작)  │ │    Failure      │
                       └──────┬──────┘ └──────┬──────┘ └──────┬──────────┘
                              │               │               │
                         백오프 후 재시도   NOT_SERVING 수신   백오프 후 재시도
                              │               │               │
                              └───────►Connecting◄─────────────┘
```

---

## 7. 동시성 모델

### RWMutex 전략

```
┌──────────────────────────────────────────────────┐
│  연산별 Lock 전략                                  │
│                                                   │
│  Check()          → RLock  (읽기 전용, 동시 가능)   │
│  Watch()          → Lock   (구독 등록/해제)         │
│  SetServingStatus → Lock   (상태 변경 + 알림)       │
│  Shutdown()       → Lock   (전체 상태 변경)         │
│  Resume()         → Lock   (전체 상태 복구)         │
│                                                   │
│  효과:                                             │
│  - 다수의 Check 요청이 동시에 처리 가능              │
│  - SetServingStatus 중에는 Check도 대기             │
│  - Watch 이벤트 루프는 Lock 없이 채널 대기           │
└──────────────────────────────────────────────────┘
```

### 채널 기반 Fan-out

```
SetServingStatus("svc", SERVING) 호출
  │
  ├─ statusMap["svc"] = SERVING
  │
  └─ for _, update := range updates["svc"]:
      │
      ├─ Client A의 채널: select { case <-ch: default: }; ch <- SERVING
      ├─ Client B의 채널: select { case <-ch: default: }; ch <- SERVING
      └─ Client C의 채널: select { case <-ch: default: }; ch <- SERVING

  각 Watch 루프:
      select {
      case status := <-update:  → stream.Send(status)
      case <-ctx.Done():        → 정리 후 종료
      }
```

이 패턴의 핵심:
1. **서버 비블로킹**: select + default로 이전 상태 폐기
2. **클라이언트 독립**: 각 클라이언트마다 개별 채널
3. **자동 정리**: defer로 구독 해제

---

## 8. Kubernetes 연동

### gRPC Health Check + K8s Probe

```yaml
apiVersion: v1
kind: Pod
spec:
  containers:
  - name: grpc-server
    livenessProbe:
      grpc:
        port: 50051
        service: ""          # 빈 문자열 = 전체 서버 상태
      initialDelaySeconds: 10
      periodSeconds: 10
    readinessProbe:
      grpc:
        port: 50051
        service: "my.service" # 특정 서비스 상태
      initialDelaySeconds: 5
      periodSeconds: 5
```

**동작 흐름:**

```
kubelet
  │
  ├─ liveness probe: Health.Check("") 호출
  │   ├─ SERVING → Pod 정상
  │   └─ NOT_SERVING/에러 → Pod 재시작
  │
  └─ readiness probe: Health.Check("my.service") 호출
      ├─ SERVING → Service 엔드포인트에 추가
      └─ NOT_SERVING/에러 → Service 엔드포인트에서 제거
```

### 로드 밸런서 연동 시나리오

```
┌─────────────────────────────────────────────────────────────┐
│  시나리오: 3개 서버 중 1개 장애                                │
│                                                             │
│  t=0   Server A: SERVING    ← 트래픽 수신 ✓                  │
│        Server B: SERVING    ← 트래픽 수신 ✓                  │
│        Server C: SERVING    ← 트래픽 수신 ✓                  │
│                                                             │
│  t=5s  Server C: DB 연결 끊김                                │
│        SetServingStatus("", NOT_SERVING)                    │
│        → Watch 클라이언트에 NOT_SERVING 전파                  │
│        → LB: Server C SubConn → TransientFailure            │
│                                                             │
│  t=6s  Server A: SERVING    ← 트래픽 수신 ✓ (50%)            │
│        Server B: SERVING    ← 트래픽 수신 ✓ (50%)            │
│        Server C: NOT_SERVING ← 트래픽 차단 ✗                 │
│                                                             │
│  t=30s Server C: DB 재연결 성공                               │
│        SetServingStatus("", SERVING)                        │
│        → Watch 클라이언트에 SERVING 전파                      │
│        → LB: Server C SubConn → Ready                       │
│                                                             │
│  t=31s Server A: SERVING    ← 트래픽 수신 ✓ (33%)            │
│        Server B: SERVING    ← 트래픽 수신 ✓ (33%)            │
│        Server C: SERVING    ← 트래픽 수신 ✓ (33%)            │
└─────────────────────────────────────────────────────────────┘
```

---

## 9. 백오프 전략

### 클라이언트 재시도 백오프

`health/client.go:37-48`

```go
var backoffStrategy = backoff.DefaultExponential
// 기본값: InitialBackoff=1s, Multiplier=1.6, MaxBackoff=120s
```

```
┌──────────────────────────────────────────────┐
│  재시도 횟수    백오프 시간     누적 대기          │
│  ─────────    ──────────    ──────────        │
│  tryCnt=0     없음 (첫 시도)  0s               │
│  tryCnt=1     ~1s            ~1s              │
│  tryCnt=2     ~1.6s          ~2.6s            │
│  tryCnt=3     ~2.56s         ~5.16s           │
│  tryCnt=4     ~4.1s          ~9.26s           │
│  ...          ...            ...              │
│  tryCnt=N     max 120s       ...              │
│                                              │
│  특수 조건: 메시지 수신 성공 시 tryCnt=0 리셋     │
│  → 서버 복구 후 즉시 빠른 재연결                  │
└──────────────────────────────────────────────┘
```

---

## 10. 전체 라이프사이클 시퀀스

```
┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐
│  Server  │     │  Health  │     │  Health  │     │  Load    │
│  App     │     │  Server  │     │ Producer │     │ Balancer │
└────┬─────┘     └────┬─────┘     └────┬─────┘     └────┬─────┘
     │                │                │                │
     │ NewServer()    │                │                │
     │───────────────►│                │                │
     │                │                │                │
     │ Register(grpcServer)            │                │
     │───────────────►│                │                │
     │                │                │                │
     │                │   Watch(svc)   │                │
     │                │◄───────────────│                │
     │                │                │                │
     │                │  SERVING       │                │
     │                │───────────────►│                │
     │                │                │  Ready         │
     │                │                │───────────────►│
     │                │                │                │
     │ SetServingStatus               │                │
     │ (svc, NOT_SERVING)             │                │
     │───────────────►│                │                │
     │                │ NOT_SERVING    │                │
     │                │───────────────►│                │
     │                │                │ TransientFailure
     │                │                │───────────────►│
     │                │                │                │
     │ SetServingStatus               │                │
     │ (svc, SERVING)                 │                │
     │───────────────►│                │                │
     │                │  SERVING       │                │
     │                │───────────────►│                │
     │                │                │  Ready         │
     │                │                │───────────────►│
     │                │                │                │
     │ Shutdown()     │                │                │
     │───────────────►│                │                │
     │                │ NOT_SERVING    │                │
     │                │───────────────►│                │
     │                │                │ TransientFailure
     │                │                │───────────────►│
```

---

## 11. 설계 인사이트 정리

### 왜 Check와 Watch를 모두 제공하는가?

| 관점 | Check (Unary) | Watch (Streaming) |
|------|--------------|-------------------|
| 복잡도 | 낮음 | 높음 |
| 네트워크 | 매번 요청/응답 | 초기 연결 후 이벤트만 |
| 지연 | 폴링 간격에 의존 | 즉시 전파 |
| 리소스 | 적음 | 스트림 유지 비용 |
| 용도 | K8s probe, CLI 도구 | 로드 밸런서, 서비스 메시 |

### 왜 서비스별 상태를 지원하는가?

하나의 gRPC 서버에 여러 서비스가 등록될 수 있다:

```
gRPC Server (:50051)
  ├─ UserService   → SERVING     (DB 연결 정상)
  ├─ OrderService  → NOT_SERVING (결제 시스템 장애)
  └─ HealthService → SERVING     (항상 SERVING)
```

서비스별 상태를 통해 **부분 장애**를 표현할 수 있다.

### 왜 Watch에서 중복 상태를 필터링하는가?

`server.go:117-119` — `lastSentStatus` 비교

빠른 상태 변동(flapping) 시 동일 상태가 연속 전송되는 것을 방지:

```
SetServingStatus("svc", NOT_SERVING)  t=0ms
SetServingStatus("svc", NOT_SERVING)  t=1ms  ← 중복, 전송 안 함
SetServingStatus("svc", SERVING)      t=2ms  ← 변경, 전송
SetServingStatus("svc", SERVING)      t=3ms  ← 중복, 전송 안 함
```

---

## 12. 참고 자료

### 소스코드 경로

| 파일 | 핵심 내용 |
|------|----------|
| `health/server.go:41-50` | Server 구조체 정의 |
| `health/server.go:52-58` | NewServer() 초기화 |
| `health/server.go:60-70` | Check() RPC |
| `health/server.go:89-132` | Watch() Streaming RPC |
| `health/server.go:136-159` | SetServingStatus() + 알림 전파 |
| `health/server.go:166-187` | Shutdown() / Resume() |
| `health/client.go:51-53` | init() — HealthCheckFunc 등록 |
| `health/client.go:59-117` | clientHealthCheck() 루프 |
| `health/producer.go:64-106` | Producer — LB 통합 |
| `health/grpc_health_v1/health.pb.go:41-48` | ServingStatus 열거형 |

### 관련 gRPC 스펙

- [GRPC Health Checking Protocol](https://github.com/grpc/grpc/blob/master/doc/health-checking.md)
- [Kubernetes gRPC Probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/#define-a-grpc-liveness-probe)
