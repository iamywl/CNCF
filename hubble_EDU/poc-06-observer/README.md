# PoC-06: Hubble 옵저버

## 개요

Hubble의 핵심 컴포넌트인 LocalObserverServer의 이벤트 루프를 시뮬레이션한다. 채널에서 MonitorEvent를 수신하여 파싱, 훅 실행, 링 버퍼 저장, GetFlows 스트리밍까지의 전체 파이프라인을 재현한다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/hubble/observer/local_observer.go` | LocalObserverServer, Start(), GetFlows(), ServerStatus() |
| `pkg/hubble/observer/observeroption/option.go` | OnMonitorEvent, OnDecodedFlow 등 훅 인터페이스 |
| `pkg/hubble/container/ring.go` | Ring 버퍼 (이벤트 저장소) |
| `pkg/hubble/filters/filters.go` | FilterFuncs, Apply() |

## 핵심 개념

### 1. Start() 이벤트 루프

```
for monitorEvent := range s.events {
    1. OnMonitorEvent 훅 실행 (전처리)
    2. payloadParser.Decode(monitorEvent) → Event
    3. if Flow:
        a. trackNamespaces(flow)
        b. OnDecodedFlow 훅 실행 (메트릭, 커스텀 처리)
        c. numObservedFlows++
    4. OnDecodedEvent 훅 실행
    5. ring.Write(ev)
}
```

훅에서 `stop=true`를 반환하면 해당 이벤트는 건너뛴다 (`continue nextEvent`).

### 2. GetFlows() 스트리밍

```
1. 필터 빌드: BuildFilterList(whitelist), BuildFilterList(blacklist)
2. 시작 위치 계산:
   - first + no since → OldestWrite() (처음부터)
   - follow + no number + no since → LastWriteParallel() (최신부터)
   - 그 외 → 역방향 탐색으로 위치 계산
3. 이벤트 반복:
   - follow? → NextFollow(ctx) 사용 (대기)
   - 시간 범위 체크 (since/until)
   - Apply(whitelist, blacklist, ev)
   - LostEvent는 필터 없이 통과
   - server.Send(response)
```

### 3. 훅 시스템

옵저버는 여러 단계에서 훅을 실행한다:
- **OnMonitorEvent**: 디코딩 전 원시 이벤트 전처리
- **OnDecodedFlow**: Flow 디코딩 후 처리 (메트릭, 정책 등)
- **OnDecodedEvent**: 모든 디코딩된 이벤트 후처리
- **OnFlowDelivery**: GetFlows에서 클라이언트 전송 전 처리

## 실행 방법

```bash
go run main.go
```

## 학습 포인트

1. **채널 기반 이벤트 루프**: `for event := range channel` 패턴
2. **훅 체인**: 각 단계에서 확장 가능한 훅 실행
3. **follow 모드**: 새 이벤트를 기다리며 실시간 스트리밍
4. **역방향 탐색**: GetFlows에서 시작 위치를 찾기 위해 링 버퍼를 역순으로 탐색
