# PoC: Hubble Observer 파이프라인

> **관련 문서**: [04-SEQUENCE-DIAGRAMS.md](../04-SEQUENCE-DIAGRAMS.md) - `hubble observe` 전체 흐름, Parser 디코딩 흐름

## 이 PoC가 보여주는 것

Hubble Observer의 **5단계 이벤트 처리 파이프라인**을 시뮬레이션합니다.

```
MonitorEvent (raw BPF 데이터)
    ↓
[1. OnMonitorEvent hooks] ← 유효성 검증, 텔레메트리
    ↓
[2. Decode (Parser)]      ← raw → Flow 변환
    ↓
[3. OnDecodedFlow hooks]  ← 메트릭 수집, enrichment
    ↓
[4. Filter]               ← whitelist/blacklist 적용
    ↓
[5. OnFlowDelivery hooks] ← 감사 로깅, 최종 변환
    ↓
클라이언트 전달
```

## 실행 방법

```bash
cd EDU/poc-observer-pipeline
go run main.go
```

## 실행하면 관찰할 수 있는 것

1. **6개 이벤트**가 파이프라인을 통과하는 과정
2. 빈 이벤트가 **OnMonitorEvent Hook에서 거부**되는 모습
3. `--verdict DROPPED` 필터로 **trace 이벤트가 필터링**되는 모습
4. DROP 이벤트만 **최종 전달**되는 결과
5. 각 단계의 **통계** (수신/디코딩/필터링/전달)

## 핵심 학습 포인트

- Hook의 `stop` 반환값이 true이면 **이후 처리 전체가 중단**
- 필터는 Decode 이후에 적용됨 (raw 이벤트에는 필터 불가)
- 메트릭은 OnDecodedFlow Hook에서 수집 (필터링과 무관하게 모든 Flow 카운트)
