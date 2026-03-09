# gRPC-Go Retry 및 Service Config 심화 분석

## 1. 개요

gRPC Service Config는 서비스별 RPC 동작을 **JSON으로 선언적**으로 정의하는 메커니즘이다. 재시도 정책(Retry Policy), 타임아웃, 로드 밸런싱 정책, 메시지 크기 제한 등을 코드 변경 없이 설정할 수 있다.

### 왜 Service Config가 필요한가?

```
┌──────────────────────────────────────────────────────────────┐
│  문제: RPC 동작을 하드코딩하면?                                 │
│                                                              │
│  1. 재시도 정책 변경 → 코드 수정 + 재배포 필요                   │
│  2. 서비스별 타임아웃 → 코드 전체에 분산                         │
│  3. LB 정책 변경 → 클라이언트 코드 수정 필요                     │
│  4. 환경별 설정 → 프로덕션/스테이징 별도 빌드                     │
│                                                              │
│  해결: Service Config JSON                                    │
│  ─ DNS TXT 레코드로 전달 → 코드 무변경 정책 변경                 │
│  ─ Resolver에서 동적 전달 → 런타임 정책 갱신                     │
│  ─ 메서드별 독립 설정 → 세밀한 제어                              │
│  ─ 재시도 스로틀링 → 자동 서버 보호                              │
└──────────────────────────────────────────────────────────────┘
```

### 소스코드 위치

```
grpc-go/
├── service_config.go              ← Service Config JSON 파싱 (269줄)
├── serviceconfig/
│   └── serviceconfig.go           ← ParseResult, Config 인터페이스
├── internal/serviceconfig/
│   └── serviceconfig.go           ← MethodConfig, RetryPolicy 구조체 (180줄)
├── stream.go                      ← retry 실행 로직 (shouldRetry, withRetry)
└── clientconn.go                  ← retryThrottler, Service Config 적용
```

---

## 2. Service Config JSON 구조

### 전체 스키마

```json
{
  "loadBalancingPolicy": "round_robin",

  "loadBalancingConfig": [
    {"round_robin": {}},
    {"pick_first": {}}
  ],

  "methodConfig": [
    {
      "name": [
        {"service": "helloworld.Greeter", "method": "SayHello"},
        {"service": "helloworld.Greeter", "method": ""}
      ],
      "waitForReady": true,
      "timeout": "10s",
      "maxRequestMessageBytes": 4194304,
      "maxResponseMessageBytes": 4194304,
      "retryPolicy": {
        "maxAttempts": 4,
        "initialBackoff": "0.1s",
        "maxBackoff": "1s",
        "backoffMultiplier": 2.0,
        "retryableStatusCodes": ["UNAVAILABLE", "RESOURCE_EXHAUSTED"]
      }
    }
  ],

  "retryThrottling": {
    "maxTokens": 10,
    "tokenRatio": 0.1
  },

  "healthCheckConfig": {
    "serviceName": ""
  }
}
```

### 필드별 상세

| 필드 | 타입 | 설명 |
|------|------|------|
| `loadBalancingPolicy` | string | 단순 LB 정책 이름 (deprecated) |
| `loadBalancingConfig` | array | LB 정책 목록 (순서대로 시도, 첫 번째 지원 정책 사용) |
| `methodConfig` | array | 메서드별 RPC 설정 |
| `retryThrottling` | object | 전역 재시도 스로틀링 |
| `healthCheckConfig` | object | 클라이언트 측 헬스체크 서비스 이름 |

---

## 3. 핵심 데이터 구조

### MethodConfig

`internal/serviceconfig/serviceconfig.go:130-152`

```go
type MethodConfig struct {
    WaitForReady *bool          // true: 연결 준비될 때까지 대기 (failFast 반대)
    Timeout      *time.Duration // 메서드별 타임아웃 (클라이언트 API 타임아웃과 min 적용)
    MaxReqSize   *int           // 요청 메시지 최대 크기 (바이트)
    MaxRespSize  *int           // 응답 메시지 최대 크기 (바이트)
    RetryPolicy  *RetryPolicy   // 재시도 정책
}
```

### RetryPolicy

`internal/serviceconfig/serviceconfig.go:157-180`

```go
type RetryPolicy struct {
    MaxAttempts          int                  // 최대 시도 횟수 (원본 + 재시도), ≥2
    InitialBackoff       time.Duration        // 첫 재시도 전 대기 시간, >0
    MaxBackoff           time.Duration        // 최대 백오프 시간, >0
    BackoffMultiplier    float64              // 지수 백오프 곱수, >0
    RetryableStatusCodes map[codes.Code]bool  // 재시도 가능한 상태 코드 집합
}
```

**RetryableStatusCodes를 왜 map으로 구현했는가?**

```
슬라이스: O(n) 검색 — 매 RPC 실패마다 순회
맵:      O(1) 검색 — 상태 코드로 즉시 판단

재시도 판단은 모든 RPC 실패 시 발생하므로 O(1)이 필수적
```

### retryThrottlingPolicy

`service_config.go:109-121`

```go
type retryThrottlingPolicy struct {
    MaxTokens float64  // 토큰 풀 최대값 (0, 1000]
    TokenRatio float64 // 성공 RPC당 추가 토큰, >0
}
```

### retryThrottler

`clientconn.go:1710-1745`

```go
type retryThrottler struct {
    max    float64    // MaxTokens
    thresh float64    // MaxTokens / 2 (임계값)
    ratio  float64    // TokenRatio
    mu     sync.Mutex
    tokens float64    // 현재 토큰 수
}
```

---

## 4. 재시도 아키텍처

### 전체 구조

```
┌────────────────────────────────────────────────────────────────┐
│                    gRPC Retry 아키텍처                           │
│                                                                │
│  ┌──────────────────────────┐                                  │
│  │     Service Config       │                                  │
│  │  ┌─────────────────────┐ │                                  │
│  │  │ methodConfig[0]     │ │                                  │
│  │  │  retryPolicy:       │ │                                  │
│  │  │   maxAttempts: 4    │ │                                  │
│  │  │   initialBackoff:0.1│ │                                  │
│  │  │   retryableCodes:   │ │                                  │
│  │  │    [UNAVAILABLE]    │ │                                  │
│  │  └─────────┬───────────┘ │                                  │
│  │  ┌─────────▼───────────┐ │                                  │
│  │  │ retryThrottling     │ │                                  │
│  │  │  maxTokens: 10      │ │                                  │
│  │  │  tokenRatio: 0.1    │ │                                  │
│  │  └─────────────────────┘ │                                  │
│  └────────────┬─────────────┘                                  │
│               │                                                │
│  ┌────────────▼─────────────┐    ┌──────────────────────────┐  │
│  │    clientStream          │    │    retryThrottler        │  │
│  │  ┌────────────────────┐  │    │  ┌──────────────────┐    │  │
│  │  │ withRetry(op)      │  │    │  │ tokens: 10.0     │    │  │
│  │  │  ├─ op(attempt)    │──┼───►│  │ thresh: 5.0      │    │  │
│  │  │  ├─ shouldRetry()  │  │    │  │ throttle()       │    │  │
│  │  │  └─ retryLocked()  │  │    │  │ successfulRPC()  │    │  │
│  │  └────────────────────┘  │    │  └──────────────────┘    │  │
│  │  ┌────────────────────┐  │    └──────────────────────────┘  │
│  │  │ replayBuffer       │  │                                  │
│  │  │ (재시도용 요청 저장)  │  │                                  │
│  │  └────────────────────┘  │                                  │
│  └──────────────────────────┘                                  │
└────────────────────────────────────────────────────────────────┘
```

### 재시도 유형

| 유형 | 조건 | 안전성 | 설명 |
|------|------|--------|------|
| **투명 재시도** | 스트림 미생성 (`transportStream == nil`) | 절대 안전 | 요청이 서버에 도달하지 않음 |
| **미처리 재시도** | `firstAttempt && unprocessed` | 안전 | 서버가 요청을 받았지만 처리하지 않음 |
| **정책 재시도** | RetryPolicy 매칭 | 조건부 | 상태 코드 + 스로틀 + 시도 횟수 확인 |

---

## 5. shouldRetry — 재시도 판단 로직

`stream.go:685-779`

gRPC의 재시도 판단은 **9단계 체크 파이프라인**으로 구현되어 있다.

```
shouldRetry(err) 호출
  │
  ├─ [1단계] 기본 체크
  │   ├─ cs.finished? → 재시도 불가 (RPC 종료)
  │   ├─ cs.committed? → 재시도 불가 (이미 커밋)
  │   └─ a.drop? → 재시도 불가 (LB가 드롭)
  │
  ├─ [2단계] 투명 재시도 확인
  │   └─ transportStream == nil && allowTransparentRetry?
  │       → Yes: 즉시 재시도 (안전)
  │
  ├─ [3단계] 미처리 확인
  │   └─ firstAttempt && transportStream.Unprocessed()?
  │       → Yes: 즉시 재시도 (서버가 처리하지 않음)
  │
  ├─ [4단계] 재시도 비활성화 확인
  │   └─ cc.dopts.disableRetry?
  │       → Yes: 재시도 불가
  │
  ├─ [5단계] 서버 푸시백 확인
  │   └─ "grpc-retry-pushback-ms" 트레일러 파싱
  │       ├─ 유효한 값 → 해당 시간만큼 대기
  │       ├─ 잘못된 값 → 재시도 불가
  │       └─ 없음 → 다음 단계로
  │
  ├─ [6단계] 상태 코드 매칭
  │   └─ RetryPolicy.RetryableStatusCodes[code]?
  │       → No: 재시도 불가
  │
  ├─ [7단계] 스로틀 확인
  │   └─ retryThrottler.throttle()?
  │       → Yes: 재시도 차단 (토큰 부족)
  │
  ├─ [8단계] 최대 시도 확인
  │   └─ numRetries+1 >= MaxAttempts?
  │       → Yes: 재시도 불가
  │
  └─ [9단계] 백오프 계산 + 대기
      ├─ 서버 푸시백 있으면: 해당 시간
      └─ 없으면: 지수 백오프 + 지터 (±20%)
          dur = min(initial * multiplier^n, max) * (0.8~1.2)
```

### 각 단계의 설계 근거

**[2단계] 투명 재시도가 최우선인 이유:**

```
시나리오: DNS 변경 후 첫 연결 시도

  attempt 1: transportStream == nil (연결 실패)
             allowTransparentRetry = true
             → 즉시 재시도 (요청이 서버에 도달하지 않았으므로 100% 안전)

  어떤 RetryPolicy도 필요 없음 — 이건 "재시도"가 아니라 "첫 시도"
```

**[5단계] 서버 푸시백을 지원하는 이유:**

```
시나리오: 서버가 캐시 워밍업 중

  클라이언트: RPC 실패 (UNAVAILABLE)
  서버 트레일러: grpc-retry-pushback-ms: 5000

  클라이언트 해석: "5초 후에 다시 시도해라"
  → 클라이언트의 자체 백오프 대신 서버 지시 시간 사용
  → numRetriesSincePushback = 0 (백오프 리셋)

  장점:
  - 서버가 직접 최적의 재시도 시점을 결정
  - 클라이언트의 고정 백오프보다 정확
  - 서버 복구 시간에 맞춤 적응
```

---

## 6. 재시도 스로틀링

### 토큰 버킷 알고리즘

```
┌────────────────────────────────────────────────────────────┐
│  retryThrottler 토큰 관리                                    │
│                                                            │
│  초기: tokens = maxTokens (예: 10.0)                         │
│  임계값: thresh = maxTokens / 2 (예: 5.0)                    │
│                                                            │
│  ┌──────────────────────────────────────────────┐           │
│  │ 토큰 풀                                      │           │
│  │  ████████████████████                        │           │
│  │  0    2    4    6    8    10                  │           │
│  │            ↑ thresh=5     ↑ max=10           │           │
│  │                                              │           │
│  │  tokens > thresh → 재시도 허용                 │           │
│  │  tokens ≤ thresh → 재시도 차단                 │           │
│  └──────────────────────────────────────────────┘           │
│                                                            │
│  실패 시: tokens -= 1  (throttle() 호출)                     │
│  성공 시: tokens += tokenRatio  (successfulRPC() 호출)       │
│                                                            │
│  예시 (maxTokens=10, tokenRatio=0.1):                       │
│  ──────────────────────────────────                         │
│  성공 10회: tokens = 10 + 10*0.1 = 11 → cap 10              │
│  실패 5회:  tokens = 10 - 5 = 5 → thresh 도달 → 차단 시작     │
│  실패 후 복구: 성공 50회 필요 (50*0.1 = 5.0 토큰 회복)         │
└────────────────────────────────────────────────────────────┘
```

### throttle() 구현

`clientconn.go:1722-1733`

```go
func (rt *retryThrottler) throttle() bool {
    if rt == nil {
        return false  // 정책 없으면 항상 허용
    }
    rt.mu.Lock()
    defer rt.mu.Unlock()
    rt.tokens--
    if rt.tokens < 0 {
        rt.tokens = 0
    }
    return rt.tokens <= rt.thresh  // true = 재시도 차단
}
```

### successfulRPC() 구현

`clientconn.go:1735-1745`

```go
func (rt *retryThrottler) successfulRPC() {
    if rt == nil {
        return
    }
    rt.mu.Lock()
    defer rt.mu.Unlock()
    rt.tokens += rt.ratio
    if rt.tokens > rt.max {
        rt.tokens = rt.max
    }
}
```

### 왜 중앙 집중식 스로틀러인가?

```
┌──────────────────────────────────────────────────────┐
│  옵션 A: 메서드별 스로틀러                              │
│  ──────────────────────                              │
│  UserService.GetUser   → throttler_1 (tokens: 8)     │
│  UserService.ListUsers → throttler_2 (tokens: 3)     │
│  OrderService.Create   → throttler_3 (tokens: 10)    │
│                                                      │
│  문제: 서버 전체가 과부하인데 한 메서드만 차단됨          │
│                                                      │
│  옵션 B: ClientConn 레벨 스로틀러 (실제 구현) ✓        │
│  ──────────────────────                              │
│  모든 RPC → throttler (tokens: 5)                     │
│                                                      │
│  장점: 서버 전체 건강 상태를 반영                        │
│  → 한 메서드의 실패가 다른 메서드의 재시도도 제한         │
│  → 서버 과부하 시 모든 재시도를 동시에 차단               │
└──────────────────────────────────────────────────────┘
```

---

## 7. 지수 백오프 + 지터

### 백오프 계산

`stream.go:760-766`

```go
fact := math.Pow(rp.BackoffMultiplier, float64(cs.numRetriesSincePushback))
cur := min(float64(rp.InitialBackoff)*fact, float64(rp.MaxBackoff))
cur *= 0.8 + 0.4*rand.Float64()  // 지터: ±20%
dur = time.Duration(int64(cur))
```

### 백오프 예시

```
설정: initialBackoff=0.1s, maxBackoff=1s, multiplier=2.0

┌──────┬───────────────┬──────────────┬──────────────────┐
│ 시도 │ 기본 백오프    │ 지터 범위     │ 실제 대기 시간    │
├──────┼───────────────┼──────────────┼──────────────────┤
│  1   │ 0.1s          │ 0.08~0.12s   │ ~0.1s            │
│  2   │ 0.2s          │ 0.16~0.24s   │ ~0.2s            │
│  3   │ 0.4s          │ 0.32~0.48s   │ ~0.4s            │
│  4   │ 0.8s          │ 0.64~0.96s   │ ~0.8s            │
│  5   │ 1.0s (cap)    │ 0.80~1.20s   │ ~1.0s            │
│  6+  │ 1.0s (cap)    │ 0.80~1.20s   │ ~1.0s            │
└──────┴───────────────┴──────────────┴──────────────────┘
```

### 왜 지터(Jitter)가 필요한가?

```
┌──────────────────────────────────────────────────────────┐
│  Thundering Herd 문제                                     │
│                                                          │
│  지터 없이 (모든 클라이언트 동시 재시도):                     │
│                                                          │
│  t=0      ████████████████  100개 클라이언트 동시 실패       │
│  t=0.1s   ████████████████  100개 동시 재시도 → 서버 과부하  │
│  t=0.3s   ████████████████  100개 동시 재시도 → 또 과부하    │
│                                                          │
│  지터 적용 (±20% 랜덤 분산):                                │
│                                                          │
│  t=0      ████████████████  100개 클라이언트 동시 실패       │
│  t=0.08s  ████                                           │
│  t=0.09s  ████████                                       │
│  t=0.10s  ████████████                                   │
│  t=0.11s  ████████                                       │
│  t=0.12s  ████              → 분산된 재시도 → 서버 안정      │
└──────────────────────────────────────────────────────────┘
```

---

## 8. Service Config 적용 흐름

### 전달 경로

```
┌───────────────────────────────────────────────────────────┐
│  Service Config 전달 경로                                   │
│                                                           │
│  방법 1: DNS TXT 레코드                                    │
│  ─────────────────────                                    │
│  DNS 쿼리: _grpc_config.my-service.example.com TXT         │
│  → DNS Resolver가 파싱 → ClientConn에 적용                  │
│                                                           │
│  방법 2: Resolver에서 직접 전달                              │
│  ─────────────────────────                                │
│  resolver.UpdateState(resolver.State{                      │
│    ServiceConfig: &serviceconfig.ParseResult{Config: sc},  │
│  })                                                       │
│                                                           │
│  방법 3: Dial Option                                       │
│  ────────────────                                         │
│  grpc.WithDefaultServiceConfig(`{...}`)                    │
│  → Resolver가 SC를 제공하지 않을 때 기본값                    │
│                                                           │
│  우선순위: Resolver > DefaultServiceConfig                   │
└───────────────────────────────────────────────────────────┘
```

### 메서드별 Config 선택

`service_config.go:1095-1107`

```
getMethodConfig(sc, "/package.Service/Method")
  │
  ├─ [1] 정확 매칭: "/package.Service/Method"
  │   → 있으면 사용
  │
  ├─ [2] 서비스 레벨: "/package.Service/"
  │   → 있으면 사용 (해당 서비스의 모든 메서드에 적용)
  │
  └─ [3] 전역 기본값: ""
      → 있으면 사용 (모든 메서드에 적용)
```

**예시:**

```json
{
  "methodConfig": [
    {
      "name": [{"service": "pkg.Svc", "method": "Critical"}],
      "retryPolicy": {"maxAttempts": 5, "retryableStatusCodes": ["UNAVAILABLE"]}
    },
    {
      "name": [{"service": "pkg.Svc"}],
      "timeout": "10s",
      "retryPolicy": {"maxAttempts": 3, "retryableStatusCodes": ["UNAVAILABLE"]}
    },
    {
      "name": [{}],
      "waitForReady": true
    }
  ]
}
```

```
pkg.Svc/Critical  → maxAttempts=5 (정확 매칭)
pkg.Svc/Normal    → maxAttempts=3, timeout=10s (서비스 매칭)
other.Svc/Any     → waitForReady=true (전역 매칭)
```

### ClientConn에 적용

`clientconn.go:1133-1154`

```go
func (cc *ClientConn) applyServiceConfigAndBalancer(sc *ServiceConfig, ...) {
    cc.sc = sc

    // retryThrottler 생성/갱신
    if cc.sc.retryThrottling != nil {
        newThrottler := &retryThrottler{
            tokens: cc.sc.retryThrottling.MaxTokens,
            max:    cc.sc.retryThrottling.MaxTokens,
            thresh: cc.sc.retryThrottling.MaxTokens / 2,
            ratio:  cc.sc.retryThrottling.TokenRatio,
        }
        cc.retryThrottler.Store(newThrottler)
    } else {
        cc.retryThrottler.Store((*retryThrottler)(nil))
    }
}
```

---

## 9. 재시도 실행 메커니즘

### withRetry — 재시도 래퍼

`stream.go:815-859`

```
withRetry(op, onSuccess)
  │
  ├─ [루프 시작]
  │   │
  │   ├─ cs.committed?
  │   │   └─ Yes → op(attempt) 직접 실행 (재시도 불가)
  │   │
  │   ├─ 첫 호출? (replayBuffer 비어있음)
  │   │   └─ newAttemptLocked(false) → csAttempt 생성
  │   │
  │   ├─ op(attempt) 실행
  │   │
  │   ├─ 성공?
  │   │   ├─ onSuccess() 호출
  │   │   └─ 반환
  │   │
  │   └─ 실패?
  │       └─ retryLocked(attempt, err)
  │           ├─ shouldRetry() → 재시도 판단
  │           ├─ newAttemptLocked() → 새 attempt 생성
  │           └─ replayBufferLocked() → 이전 작업 재실행
  │
  └─ [루프 반복]
```

### replayBuffer — 재시도를 위한 요청 저장

```
┌──────────────────────────────────────────────────────────┐
│  replayBuffer 메커니즘                                     │
│                                                          │
│  첫 시도:                                                 │
│    op1: SendMsg(req1) → replayBuffer에 기록               │
│    op2: SendMsg(req2) → replayBuffer에 기록               │
│    op3: RecvMsg() → 실패!                                 │
│                                                          │
│  재시도:                                                  │
│    replayBufferLocked():                                 │
│      op1: SendMsg(req1) 재실행                             │
│      op2: SendMsg(req2) 재실행                             │
│    → 이전 상태까지 복원 후 계속                              │
│                                                          │
│  커밋 시:                                                 │
│    commitAttemptLocked():                                │
│      replayBuffer = nil → 메모리 해제                      │
│      cs.committed = true → 더 이상 재시도 불가              │
└──────────────────────────────────────────────────────────┘
```

### 커밋(Commit) 시점

재시도가 더 이상 불가능해지는 시점:

```
커밋 조건:
  1. 응답의 첫 번째 데이터 수신 (서버 스트리밍)
  2. shouldRetry()가 false 반환 (최대 시도 초과, 스로틀, 비재시도 코드)
  3. 클라이언트가 CloseSend() 호출 후 서버 응답 시작

커밋 시 동작:
  - onCommit() 콜백 호출
  - replayBuffer 정리 (cleanup 호출)
  - cs.committed = true
```

---

## 10. Service Config 검증

### JSON 파싱 및 검증

`service_config.go:172-269`

```
JSON 입력
  │
  ├─ json.Unmarshal() → jsonSC 구조체
  │
  ├─ methodConfig 파싱
  │   ├─ name 배열 → "/service/method" 키 생성
  │   ├─ WaitForReady, Timeout, MaxReqSize, MaxRespSize 추출
  │   └─ retryPolicy 검증:
  │       ├─ MaxAttempts ≥ 2?
  │       ├─ InitialBackoff > 0?
  │       ├─ MaxBackoff > 0?
  │       ├─ BackoffMultiplier > 0?
  │       └─ RetryableStatusCodes 비어있지 않음?
  │
  ├─ retryThrottling 검증
  │   ├─ MaxTokens ∈ (0, 1000]?
  │   └─ TokenRatio > 0?
  │
  └─ ServiceConfig 반환 또는 에러
```

### 검증 실패 사례

```go
// MaxAttempts < 2 → 에러
{"retryPolicy": {"maxAttempts": 1}}
// → "maxAttempts must be at least 2"

// InitialBackoff ≤ 0 → 에러
{"retryPolicy": {"initialBackoff": "0s"}}
// → "initialBackoff must be positive"

// RetryableStatusCodes 비어있음 → 에러
{"retryPolicy": {"retryableStatusCodes": []}}
// → "retryableStatusCodes must be non-empty"

// MaxTokens > 1000 → 에러
{"retryThrottling": {"maxTokens": 1001}}
// → "maxTokens must be in (0, 1000]"
```

---

## 11. HedgingPolicy 상태

`stream.go:618`의 TODO 주석에 따르면, HedgingPolicy는 **현재 미구현**이다:

```go
// TODO(hedging): hedging will have multiple attempts simultaneously.
```

**Hedging vs Retry:**

```
┌──────────────────────────────────────────────────────────┐
│  Retry (구현됨)                                           │
│  ─────────────                                           │
│  시도 1 → 실패 → 대기 → 시도 2 → 실패 → 대기 → 시도 3      │
│  (순차적, 이전 실패 후에만 다음 시도)                         │
│                                                          │
│  Hedging (미구현)                                         │
│  ──────────────                                          │
│  시도 1 ───────────────────────►                          │
│  시도 2 ──────────►  (hedge delay 후)                     │
│  시도 3 ───►         (hedge delay 후)                     │
│  → 먼저 성공한 응답 사용, 나머지 취소                        │
│  (동시 발사, 가장 빠른 응답 채택)                            │
│                                                          │
│  Hedging 장점: 꼬리 지연(tail latency) 감소                 │
│  Hedging 단점: 서버 부하 증가                               │
└──────────────────────────────────────────────────────────┘
```

---

## 12. 전체 흐름 시퀀스

```
┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐
│ Client   │     │clientConn│     │  Server  │     │  retry   │
│ App      │     │ stream   │     │          │     │ Throttler│
└────┬─────┘     └────┬─────┘     └────┬─────┘     └────┬─────┘
     │                │                │                │
     │ Invoke(method) │                │                │
     │───────────────►│                │                │
     │                │                │                │
     │         getMethodConfig()       │                │
     │                │ RetryPolicy    │                │
     │                │ maxAttempts=4  │                │
     │                │                │                │
     │         withRetry(sendMsg)      │                │
     │                │                │                │
     │         attempt 1               │                │
     │                │── sendMsg ────►│                │
     │                │◄── UNAVAILABLE │                │
     │                │                │                │
     │         shouldRetry()           │                │
     │                │                │  throttle()    │
     │                │────────────────┼───────────────►│
     │                │                │  tokens: 9→8   │
     │                │◄───────────────┼──── false      │
     │                │                │   (허용)        │
     │                │                │                │
     │         백오프 대기 (0.1s ±20%)  │                │
     │                │                │                │
     │         attempt 2               │                │
     │                │── sendMsg ────►│                │
     │                │◄── OK          │                │
     │                │                │ successfulRPC()│
     │                │────────────────┼───────────────►│
     │                │                │ tokens: 8+0.1  │
     │                │                │                │
     │◄── 응답 반환 ───│                │                │
```

---

## 13. 실용적 설정 가이드

### 일반적인 재시도 설정

```json
{
  "methodConfig": [{
    "name": [{"service": ""}],
    "waitForReady": true,
    "retryPolicy": {
      "maxAttempts": 3,
      "initialBackoff": "0.1s",
      "maxBackoff": "1s",
      "backoffMultiplier": 2.0,
      "retryableStatusCodes": ["UNAVAILABLE"]
    }
  }],
  "retryThrottling": {
    "maxTokens": 10,
    "tokenRatio": 0.1
  }
}
```

### 용도별 권장 설정

| 용도 | maxAttempts | initialBackoff | retryableStatusCodes |
|------|------------|----------------|---------------------|
| 일반 API | 3 | 0.1s | UNAVAILABLE |
| 중요 트랜잭션 | 5 | 0.5s | UNAVAILABLE, RESOURCE_EXHAUSTED |
| 빠른 실패 | 2 | 0.01s | UNAVAILABLE |
| 배치 처리 | 4 | 1s | UNAVAILABLE, DEADLINE_EXCEEDED |

### 재시도하면 안 되는 상태 코드

| 코드 | 이유 |
|------|------|
| INVALID_ARGUMENT | 클라이언트 버그, 재시도해도 동일 |
| NOT_FOUND | 리소스 없음, 재시도 무의미 |
| ALREADY_EXISTS | 중복, 재시도 시 부작용 |
| PERMISSION_DENIED | 인가 실패, 재시도 무의미 |
| UNAUTHENTICATED | 인증 실패, 토큰 갱신 필요 |
| UNIMPLEMENTED | 서버 미구현, 재시도 무의미 |

---

## 14. 설계 인사이트 정리

### 왜 RetryPolicy와 HedgingPolicy를 상호 배타적으로 설계했는가?

하나의 메서드에 retry와 hedging을 동시에 적용하면 동작이 모호해진다:
- retry는 "실패 후 재시도", hedging은 "동시 발사"
- 두 정책이 충돌하면 시도 횟수가 폭발적으로 증가
- 따라서 spec 레벨에서 둘 중 하나만 선택하도록 강제

### 왜 replayBuffer로 구현했는가?

```
대안 1: 요청 메시지를 복사해서 저장
  → 큰 메시지의 경우 메모리 낭비

대안 2: 원본 메시지 참조 저장
  → 사용자가 메시지를 수정하면 재시도 시 달라짐

실제 구현: 연산(operation) 자체를 저장
  → SendMsg, CloseSend 등의 작업을 순서대로 재실행
  → 메시지 내용이 아닌 "무엇을 했는가"를 기록
  → cleanup 콜백으로 리소스 해제
```

### 왜 커밋(commit) 개념이 필요한가?

```
문제: 서버가 응답 데이터를 보내기 시작한 후 재시도하면?
  → 클라이언트가 일부 응답을 받은 상태에서 처음부터 다시 시작
  → 중복 데이터, 일관성 파괴

해결: 커밋 시점 이후에는 재시도 불가
  → 첫 응답 데이터 수신 = 커밋
  → replayBuffer 해제 (메모리 절약)
  → 이후 오류는 클라이언트에 그대로 전달
```

---

## 15. 참고 자료

### 소스코드 경로

| 파일 | 핵심 내용 |
|------|----------|
| `service_config.go:109-121` | retryThrottlingPolicy 구조체 |
| `service_config.go:158-164` | ServiceConfig 구조체 |
| `service_config.go:172-269` | JSON 파싱 및 검증 |
| `internal/serviceconfig/serviceconfig.go:130-152` | MethodConfig 구조체 |
| `internal/serviceconfig/serviceconfig.go:157-180` | RetryPolicy 구조체 |
| `stream.go:618` | HedgingPolicy TODO 주석 |
| `stream.go:685-779` | shouldRetry() — 9단계 재시도 판단 |
| `stream.go:782-803` | retryLocked() — 재시도 실행 |
| `stream.go:815-859` | withRetry() — 재시도 래퍼 |
| `clientconn.go:1133-1154` | Service Config 적용 |
| `clientconn.go:1710-1745` | retryThrottler 구현 |

### 관련 스펙

- [gRPC Service Config](https://github.com/grpc/grpc/blob/master/doc/service_config.md)
- [gRPC Retry Design](https://github.com/grpc/proposal/blob/master/A6-client-retries.md)
- [gRPC Hedging](https://github.com/grpc/proposal/blob/master/A6-client-retries.md#hedged-requests)
