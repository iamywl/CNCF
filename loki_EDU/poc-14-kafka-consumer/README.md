# PoC #14: Kafka 소비자 - 파티션 기반 로그 소비 및 블록 빌더

## 개요

Loki의 Kafka 기반 수집 경로를 시뮬레이션한다. Distributor가 로그를 Kafka 파티션에 쓰면, Block Builder(또는 Ingester)가 파티션에서 로그를 읽어 청크를 구성하고 스토리지에 플러시하는 과정을 재현한다.

## 아키텍처

```
                    Kafka Topic (loki-logs)
                    ┌──────────────────────┐
Producer ──────────→│ Partition 0          │──→ Consumer (ingester-1)
(Distributor)       │ Partition 1          │      ↓
  │                 │ Partition 2          │   BlockBuilder
  │ hash(tenantID)  │ Partition 3          │──→ Consumer (ingester-2)
  │ % partitions    │ Partition 4          │      ↓
  └─────────────────│ Partition 5          │   BlockBuilder
                    └──────────────────────┘
                                                  ↓
                                             Chunk Flush → Storage
```

## 핵심 개념

### 파티션 라우팅
- 테넌트 ID의 FNV 해시로 파티션 결정
- 같은 테넌트의 로그는 항상 같은 파티션 → 순서 보장

### 블록 빌더 흐름
```
읽기 → 누적(Accumulate) → 빌드(Build) → 플러시(Flush) → 오프셋 커밋
                ↓
        테넌트+레이블별 버퍼링
        maxEntries 도달 시 자동 빌드
```

### 리밸런싱
- 컨슈머 추가/제거 시 파티션 재할당
- Range Assignor 전략: 파티션을 균등 분배

## 실행 방법

```bash
go run main.go
```

## 시뮬레이션 내용

1. **Kafka 토픽/파티션 생성**: 6개 파티션
2. **프로듀서 라우팅**: 테넌트 ID 해싱으로 파티션 할당
3. **컨슈머 그룹**: 2명의 멤버에게 파티션 분배
4. **블록 빌더**: 엔트리 누적 → 청크 빌드 → 플러시
5. **리밸런싱**: 멤버 추가/제거 시 파티션 재할당
6. **오프셋 관리**: 파티션별 처리 위치 추적

## Loki 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/dataobj/consumer/processor.go` | Kafka 레코드 처리 |
| `pkg/dataobj/consumer/flush.go` | 청크 플러시 로직 |
| `pkg/dataobj/consumer/flush_manager.go` | 플러시 매니저 |
| `pkg/dataobj/consumer/service.go` | 컨슈머 서비스 |
