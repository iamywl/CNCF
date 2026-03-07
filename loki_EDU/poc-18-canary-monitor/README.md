# PoC #18: 카나리 모니터 - Loki Canary 기반 로그 파이프라인 헬스체크

## 개요

Loki Canary(`cmd/loki-canary/`)의 로그 파이프라인 건강 상태 모니터링 메커니즘을 시뮬레이션한다. 알려진 로그 엔트리를 주기적으로 생성하고 쿼리하여 누락, 지연, 일관성을 측정한다.

## 아키텍처

```
┌─────────────┐     ┌─────────────────────────┐     ┌──────────────┐
│ Canary      │     │     Loki Pipeline        │     │ Canary       │
│ Writer      │────→│ Distributor → Ingester   │────→│ Reader       │
│             │     │   → Storage → Querier    │     │              │
│ sentChan ───┼─────┼──────────────────────────┼─────┼→ receivedChan│
└─────────────┘     └─────────────────────────┘     └──────────────┘
       │                                                    │
       └──────────────────┐ ┌───────────────────────────────┘
                          ▼ ▼
                    ┌─────────────┐
                    │ Comparator  │
                    │             │
                    │ - 누락 감지  │
                    │ - 지연 측정  │
                    │ - Spot Check│
                    │ - Metric    │
                    └─────────────┘
```

## 핵심 개념

### Canary Writer
- 설정된 간격(기본 1초)으로 시퀀스 번호가 포함된 로그 생성
- `sentChan`으로 전송 시간 기록

### Canary Reader
- Loki API를 주기적으로 쿼리하여 카나리 엔트리 수신
- WebSocket 또는 HTTP 쿼리 사용

### Comparator
- 전송/수신 비교로 누락 감지
- 지연 시간 히스토그램 생성
- `pruneInterval`(기본 60초) 간격으로 비교 수행

### Spot Check
- 과거 엔트리를 무작위로 다시 검증
- 장기적 데이터 무결성 확인
- `spot-check-interval` 간격으로 하나씩 저장

## 실행 방법

```bash
go run main.go
```

## 시뮬레이션 내용

1. **카나리 설정**: 드롭 비율, 지연, 쓰기 간격
2. **엔트리 생성**: 시퀀스 번호가 포함된 50개 엔트리
3. **쿼리 및 수신**: 파이프라인에서 엔트리 검색
4. **비교 분석**: 누락률, 지연 통계, 히스토그램
5. **스팟 체크**: 과거 엔트리 무작위 재검증
6. **파이프라인 시나리오**: 정상/경미/심각/장애 상태 비교

## Loki 소스코드 참조

| 파일 | 역할 |
|------|------|
| `cmd/loki-canary/main.go` | 카나리 진입점, 설정 파싱 |
| `pkg/canary/writer/writer.go` | 카나리 로그 생성 |
| `pkg/canary/reader/reader.go` | 카나리 로그 쿼리 |
| `pkg/canary/comparator/comparator.go` | 전송/수신 비교, 메트릭 |

## 주요 설정 (Loki 기본값)

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `interval` | 1s | 로그 생성 간격 |
| `size` | 100 | 로그 라인 크기 (bytes) |
| `wait` | 60s | 수신 대기 시간 |
| `max-wait` | 5m | 최대 대기 (이후 누락 판정) |
| `pruneinterval` | 60s | 비교 수행 간격 |
| `spot-check-interval` | 15m | 스팟 체크 엔트리 저장 간격 |
| `spot-check-max` | 4h | 스팟 체크 최대 보존 시간 |
| `buckets` | 10 | 지연 히스토그램 버킷 수 |
