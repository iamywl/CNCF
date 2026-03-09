# PoC-21: MirrorMaker 2.0 시뮬레이션

## 관련 문서
- [24-mirrormaker.md](../../kafka_EDU/24-mirrormaker.md)

## 시뮬레이션 내용
1. **MirrorSourceConnector**: 원본→대상 클러스터 실시간 데이터 복제 (토픽 필터링)
2. **ReplicationPolicy**: source.topic 이름 변환 (us-east.orders)
3. **OffsetSyncStore**: 원본↔대상 오프셋 매핑 및 변환
4. **MirrorCheckpointConnector**: Consumer Group 오프셋 동기화
5. **MirrorHeartbeatConnector**: 클러스터 간 연결 상태 확인

## 참조 소스
- `connect/mirror/src/main/java/.../MirrorSourceConnector.java`
- `connect/mirror/src/main/java/.../MirrorCheckpointConnector.java`
- `connect/mirror/src/main/java/.../OffsetSyncStore.java`

## 실행
```bash
go run main.go
```
