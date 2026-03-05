# PoC-10: 이벤트 Publisher/Subscriber 시뮬레이션

## 개요

containerd의 이벤트 시스템(Exchange, Envelope, 필터링, Forward)을 시뮬레이션한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| Exchange | `core/events/exchange/exchange.go` (Broadcaster 기반) | 구독자 목록 + broadcast 메서드 |
| Envelope | `core/events/events.go` (Timestamp, Namespace, Topic, Event) | 동일 구조체 |
| Publisher | `core/events/events.go` (Publish 인터페이스) | Exchange.Publish() |
| Subscriber | `core/events/events.go` (Subscribe 인터페이스) | Exchange.Subscribe() + 채널 |
| Forwarder | `core/events/events.go` (Forward 인터페이스) | Exchange.Forward() |
| 필터링 | `pkg/filters` + `goevents.NewFilter` | 토픽 글로브 매칭 |

## 핵심 개념

### 이벤트 아키텍처
```
  ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
  │  API Server │     │ Image Store │     │    Shim     │
  │  (Publish)  │     │  (Publish)  │     │  (Forward)  │
  └──────┬──────┘     └──────┬──────┘     └──────┬──────┘
         │                   │                    │
         └───────────────────┼────────────────────┘
                             │
                      ┌──────▼──────┐
                      │  Exchange   │
                      │ (Broadcast) │
                      └──────┬──────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
       ┌──────▼──────┐ ┌────▼────┐ ┌───────▼───────┐
       │ Subscriber  │ │ Filter  │ │   GC/Events   │
       │ (gRPC)      │ │(/tasks/*)│ │(metadata 변경) │
       └─────────────┘ └─────────┘ └───────────────┘
```

### Publish vs Forward
```
Publish:                          Forward:
  ctx → namespace 추출               이미 완성된 Envelope 전달
  topic 유효성 검증                   Envelope 유효성 검증
  Event 직렬화 (typeurl)              (이미 직렬화됨)
  Envelope 생성                      바로 broadcast
  broadcaster.Write()                broadcaster.Write()

용도: containerd 내부 이벤트          용도: shim → containerd 이벤트
```

### 토픽 기반 필터링
```
"/containers/*"     → /containers/create, /containers/delete 매칭
"/images/delete"    → /images/delete만 매칭
"/tasks/*"          → /tasks/start, /tasks/exit 매칭
(필터 없음)          → 모든 이벤트 수신
```

## 소스 참조

| 파일 | 핵심 구조체/함수 |
|------|----------------|
| `core/events/events.go` | `Envelope`, `Publisher`, `Forwarder`, `Subscriber` 인터페이스 |
| `core/events/exchange/exchange.go` | `Exchange`, `Publish()`, `Forward()`, `Subscribe()` |
| `pkg/namespaces/context.go` | `WithNamespace()`, `NamespaceRequired()` |
| `core/metadata/db.go` | `publishEvents()` — GC 후 이벤트 발행 |

## 실행

```bash
go run main.go
```

## 예상 출력

```
=== containerd 이벤트 Publisher/Subscriber 시뮬레이션 ===

--- 데모 1: 기본 Publish/Subscribe ---
구독자 1: 필터 없음 (모든 이벤트 수신)
수신된 이벤트: 3건
  [...] ns=default topic=/containers/create event=ContainerCreate{id=c1, image=nginx}
  [...] ns=default topic=/containers/delete event=ContainerDelete{id=c1}
  [...] ns=default topic=/images/create event=ImageCreate{name=nginx:latest}

--- 데모 2: 토픽 기반 필터링 ---
구독자 2 수신 (/containers/*): 2건
구독자 3 수신 (/images/delete): 1건

--- 데모 3: Forward — shim에서 containerd로 이벤트 전달 ---
Forward 수신 (/tasks/*): 2건

--- 데모 5: Publish 유효성 검증 ---
네임스페이스 없는 Publish: failed publishing event: namespace is required
빈 토픽 Publish: envelope topic "": topic must not be empty

--- 데모 6: 다중 구독자 동시 수신 ---
구독자 1: 10건 수신
구독자 2: 10건 수신
구독자 3: 10건 수신
=> 모든 구독자가 동일한 이벤트를 수신 (Broadcast)
```
