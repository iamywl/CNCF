# PoC-15: Linearizable Read (선형 읽기)

## 개요

etcd의 ReadIndex 프로토콜을 시뮬레이션한다. Serializable Read(로컬 즉시 읽기)와 Linearizable Read(리더 확인 후 최신 데이터 읽기)의 차이를 보여주고, 읽기 요청 배칭 최적화를 구현한다.

## etcd 소스 참조

- `server/etcdserver/v3_server.go` - `linearizableReadLoop()`, `linearizableReadNotify()`, `requestCurrentIndex()`
- `server/etcdserver/v3_server.go:53` - `readIndexRetryTime = 500ms`

## 핵심 개념

### ReadIndex 프로토콜 흐름
1. 클라이언트가 linearizableReadNotify() 호출
2. readwaitc 채널로 readLoop에 신호
3. readNotifier 교체 (배칭 경계)
4. requestCurrentIndex() → 리더에 ReadIndex 전송
5. 리더가 과반수 heartbeat 전송 → 자격 확인
6. commitIndex 반환 → appliedIndex >= commitIndex 대기
7. 읽기 허용 → notifier로 결과 전파

### 배칭 최적화
- 여러 동시 읽기 요청이 같은 notifier를 공유
- readLoop가 notifier를 교체하는 순간이 배칭 경계
- 하나의 ReadIndex 호출로 여러 읽기 요청 처리

## 실행 방법

```bash
go run main.go
```

## 데모 내용

1. Serializable vs Linearizable 읽기 차이
2. Stale Read 시연
3. 읽기 요청 배칭
4. 노드 장애 시 선형 읽기
5. ReadIndex 프로토콜 단계별 추적
