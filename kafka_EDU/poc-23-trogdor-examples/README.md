# PoC-23: Trogdor 테스트 프레임워크 + Examples 시뮬레이션

## 관련 문서
- [26-trogdor-examples.md](../../kafka_EDU/26-trogdor-examples.md)

## 시뮬레이션 내용
1. **TaskSpec 다형성**: ProduceBenchSpec, ConsumeBenchSpec, NetworkPartitionFaultSpec → TaskController + TaskWorker
2. **TaskManager 상태 머신**: PENDING → RUNNING → STOPPING → DONE (단일 스레드 이벤트 루프)
3. **ShutdownManager**: 참조 카운팅(Reference) 기반 안전한 graceful shutdown
4. **WorkerManager**: Worker 생명주기 (STARTING → RUNNING → STOPPING → DONE)
5. **Coordinator → Agent REST 통신**: 분산 태스크 배포 및 상태 폴링
6. **NetworkPartitionFault**: 네트워크 파티션 장애 주입 (iptables 시뮬레이션)
7. **Examples - Producer-Consumer**: CountDownLatch(WaitGroup) 패턴

## 참조 소스
- `trogdor/src/main/java/.../coordinator/TaskManager.java`
- `trogdor/src/main/java/.../agent/WorkerManager.java`
- `trogdor/src/main/java/.../task/TaskSpec.java`
- `trogdor/src/main/java/.../workload/ProduceBenchSpec.java`
- `trogdor/src/main/java/.../fault/NetworkPartitionFaultSpec.java`
- `examples/src/main/java/kafka/examples/KafkaConsumerProducerDemo.java`

## 실행
```bash
go run main.go
```
