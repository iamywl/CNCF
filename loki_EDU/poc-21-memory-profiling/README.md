# PoC-21: 메모리 관리 & 프로파일링 시뮬레이션

## 개요

Loki의 메모리 관리(`pkg/memory/`)와 트레이싱(`pkg/tracing/`)의 핵심 개념을 시뮬레이션한다.

## 시뮬레이션 항목

| 개념 | 소스 참조 | 시뮬레이션 방법 |
|------|----------|---------------|
| Arena Allocator | `pkg/memory/allocator.go` | Region 풀 + Bitmap 추적 |
| Bitmap | `pkg/memory/bitmap.go` | LSB 순서 비트 패킹 |
| Parent-Child 계층 | `pkg/memory/allocator.go` | 부모-자식 영역 빌림/반환 |
| 64바이트 정렬 | `pkg/memory/internal/memalign/` | align64() 구현 |
| CAS 동시성 검증 | `pkg/memory/allocator.go` | atomic.Bool + goroutine별 할당기 |
| 트레이싱 설정 | `pkg/tracing/config.go` | Config + RegisterFlags |
| OTel KV 변환 | `pkg/tracing/otel_kv.go` | KeyValuesToAttributes() |

## 실행

```bash
go run main.go
```

## 핵심 출력

- Bitmap LSB 비트 연산 및 IterValues 순회
- Arena Allocator의 Allocate/Reclaim/Trim 동작
- Parent-Child 할당기 영역 빌림/반환 메커니즘
- 64바이트 정렬 변환 테이블
- Reset vs Free 동작 비교
- 메모리 사용 통계 (활용률, 재사용 가능 바이트)
