# PoC #10: WAL - Write-Ahead Log 기반 내구성 보장

## 개요

Loki의 Ingester는 메모리에 로그를 버퍼링한 뒤 주기적으로 청크로 플러시한다. WAL은 프로세스 비정상 종료 시 메모리 데이터 손실을 방지하는 핵심 메커니즘이다.

## 실제 Loki 코드와의 관계

| 이 PoC | Loki 실제 코드 |
|--------|---------------|
| WAL 세그먼트 | `pkg/ingester/wal/` (Prometheus TSDB WAL 기반) |
| Record 인코딩 | `pkg/ingester/wal/record.go` |
| Checkpoint | `pkg/ingester/checkpoint.go` |
| Recovery | `pkg/ingester/recovery.go` |
| CRC32 검증 | TSDB WAL의 레코드 무결성 검증 |

## 핵심 메커니즘

### WAL 기록 흐름
```
Push 요청 → [1] WAL 세그먼트 기록 (순차 쓰기)
          → [2] 메모리에 적용
```

### 레코드 바이너리 형식
```
[Type(1B)] [DataLen(4B)] [Data(가변)] [CRC32(4B)]
```

### Checkpoint + Recovery
```
복구 과정:
1. 최신 checkpoint 로드 (전체 스냅샷)
2. checkpoint 이후의 세그먼트만 리플레이
3. counter 기반 중복 제거
```

## 실행 방법

```bash
go run main.go
```

## 시연 내용

1. **WAL 기본 기록**: Push 요청 → WAL 기록 → 메모리 적용
2. **WAL 복구**: 비정상 종료 시뮬레이션 → 세그먼트 리플레이
3. **Checkpoint + Recovery**: 체크포인트 생성 → 이후 세그먼트만 리플레이
4. **세그먼트 로테이션**: 세그먼트 최대 크기 초과 시 새 세그먼트 생성
5. **CRC32 무결성 검증**: 정상/손상 레코드 디코딩 비교

## 핵심 설계 포인트

- **WAL-first**: 항상 WAL에 먼저 기록 후 메모리에 적용 (내구성 보장)
- **순차 쓰기**: Append-only로 높은 쓰기 성능 (랜덤 I/O 없음)
- **CRC32 체크섬**: 각 레코드의 데이터 무결성 보장
- **Checkpoint**: 주기적 전체 스냅샷으로 복구 시간 단축
- **Counter dedup**: 전역 카운터로 체크포인트/세그먼트 간 중복 방지
