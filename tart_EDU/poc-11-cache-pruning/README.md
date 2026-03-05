# PoC-11: LRU 기반 캐시 정리와 자동 프루닝 시뮬레이션

## 개요

tart는 OCI 캐시와 IPSW 캐시를 효율적으로 관리하기 위해 세 가지 프루닝 전략을 제공한다.
이 PoC는 Prunable 인터페이스, 기간/예산/자동 프루닝, 참조 카운트 GC를 Go로 시뮬레이션한다.

### 핵심 시뮬레이션 포인트

1. **Prunable 인터페이스**: AccessDate(), AllocatedSize(), Delete() — 캐시 항목 추상화
2. **pruneOlderThan**: N일 이상 미접근 항목 일괄 삭제
3. **pruneSpaceBudget**: LRU 정렬 후 용량 예산 초과 항목 삭제 (최신 항목 우선 유지)
4. **reclaimIfNeeded**: 디스크 가용 용량 부족 시 자동 LRU 삭제 (initiator 보호)
5. **GC**: 참조 카운트 0인 OCI 레이어 가비지 컬렉션
6. **시간 흐름 시뮬레이션**: 30일간 캐시 생성/접근/프루닝 반복

## 실행 방법

```bash
cd tart_EDU/poc-11-cache-pruning
go run main.go
```

## 핵심 알고리즘

```
pruneSpaceBudget(budget):
  1. 모든 prunable을 최신 접근순으로 정렬
  2. 순회하며 budget에서 allocatedSize 차감
  3. budget < 0 이 되는 항목부터 삭제 대상

reclaimIfNeeded(requiredBytes):
  1. 디스크 가용 용량 확인
  2. requiredBytes < available이면 리턴
  3. reclaimBytes = required - available
  4. LRU 순서(오래된 것부터)로 reclaimBytes만큼 삭제
  5. initiator(자기 자신)는 삭제 대상에서 제외
```

## tart 실제 소스코드 참조

| 파일 | 설명 |
|------|------|
| `Sources/tart/Prunable.swift` | Prunable, PrunableStorage 프로토콜 정의 |
| `Sources/tart/URL+Prunable.swift` | URL 확장 — allocatedSizeBytes, sizeBytes, delete 구현 |
| `Sources/tart/Commands/Prune.swift` | pruneOlderThan, pruneSpaceBudget, reclaimIfNeeded 구현 |
| `Sources/tart/VMStorageOCI.swift` | OCI 캐시 저장소, gc() 메서드 |
| `Sources/tart/IPSWCache.swift` | IPSW 복원 이미지 캐시 저장소 |

## 프루닝 전략 비교

| 전략 | 기준 | 사용 시점 | tart 옵션 |
|------|------|----------|-----------|
| pruneOlderThan | 마지막 접근 시간 | 수동 (`tart prune --older-than=7`) | `--older-than` |
| pruneSpaceBudget | 용량 예산 | 수동 (`tart prune --space-budget=50`) | `--space-budget` |
| reclaimIfNeeded | 디스크 가용 용량 | 자동 (pull/clone 시) | 자동 (TART_NO_AUTO_PRUNE으로 비활성화) |
| GC | 참조 카운트 | 자동 (명령 실행 전) | `tart prune --gc` |
