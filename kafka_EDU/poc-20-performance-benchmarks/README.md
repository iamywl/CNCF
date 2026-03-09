# PoC-20: Kafka 성능 벤치마크 시뮬레이션

## 관련 문서
- [23-performance-benchmarks.md](../../kafka_EDU/23-performance-benchmarks.md)

## 시뮬레이션 내용
1. **Producer 벤치마크**: 처리량(records/sec, MB/sec) + 레이턴시 측정
2. **Consumer 벤치마크**: 소비 처리량 + Fetch 레이턴시
3. **Throttle**: 주기 기반 처리량 제한 (Trogdor Throttle 구현)
4. **Histogram**: P50/P95/P99 백분위수 레이턴시
5. **End-to-End 레이턴시**: 생산에서 소비까지 전체 경로
6. **레코드 크기별 비교**: 다양한 payload 크기의 성능 차이

## 참조 소스
- `tools/src/main/java/org/apache/kafka/tools/ProducerPerformance.java`
- `tools/src/main/java/org/apache/kafka/tools/ConsumerPerformance.java`
- `trogdor/src/main/java/.../workload/Throttle.java`

## 실행
```bash
go run main.go
```
