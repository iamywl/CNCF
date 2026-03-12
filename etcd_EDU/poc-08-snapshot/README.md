# PoC-08: 상태 스냅샷 생성/복구

## 개요

etcd의 스냅샷 시스템을 시뮬레이션한다.
KV 상태 직렬화, CRC32 무결성 검증, WAL+스냅샷 기반 복구를 재현한다.

## 핵심 개념

| 개념 | 설명 | etcd 소스 |
|------|------|-----------|
| Snapshotter | 스냅샷 저장/로드 관리 | `server/etcdserver/api/snap/snapshotter.go` |
| CRC32 | Castagnoli 알고리즘 무결성 검증 | `snap.crcTable` |
| 파일명 | `{term:016x}-{index:016x}.snap` | `Snapshotter.save()` |
| WAL | Write-Ahead Log, 변경 이력 기록 | `server/storage/wal/wal.go` |
| 복구 | 스냅샷 복원 + WAL 재생 | 서버 시작 시 복구 흐름 |

## 실행

```bash
go run main.go
```

## 시뮬레이션 시나리오

1. **초기 데이터 입력**: 5개 키 생성 + WAL 기록
2. **스냅샷 생성**: 전체 KV 상태 직렬화 → CRC32 → 파일 저장
3. **추가 데이터**: 스냅샷 이후 변경 (WAL에만 기록)
4. **크래시 시뮬레이션**: 메모리 상태 손실
5. **복구**: 스냅샷 로드 → CRC 검증 → WAL 재생 → 최신 상태 도달
6. **손상 감지**: 파일 변조 시 CRC 불일치 에러
7. **성능 측정**: 10,000키 스냅샷 직렬화/저장/로드 시간

## 참조 소스

- `server/etcdserver/api/snap/snapshotter.go` - 스냅샷 저장/로드
- `server/etcdserver/api/snap/snappb/snap.pb.go` - 스냅샷 메시지 구조
- `server/storage/wal/wal.go` - Write-Ahead Log
