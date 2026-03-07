# PoC: Jaeger Storage Factory 패턴

## 개요

Jaeger의 **Storage Factory 패턴**을 시뮬레이션한다.
Jaeger는 다양한 스토리지 백엔드(Memory, Cassandra, Elasticsearch, Badger 등)를
일관된 인터페이스로 추상화하기 위해 Factory 패턴을 사용한다.

## 시뮬레이션 대상

| Jaeger 소스 경로 | 시뮬레이션 내용 |
|------------------|----------------|
| `internal/storage/v2/api/tracestore/factory.go` | Factory 인터페이스 |
| `internal/storage/v2/api/tracestore/reader.go` | Reader 인터페이스 (GetTrace, FindTraces, GetServices, GetOperations) |
| `internal/storage/v2/api/tracestore/writer.go` | Writer 인터페이스 (WriteSpan) |
| `internal/storage/v2/memory/factory.go` | MemoryFactory 구현 |
| `internal/storage/v2/memory/memory.go` | MemoryStore (Reader + Writer) |
| `docs/adr/003-lazy-storage-factory-initialization.md` | 지연 초기화 패턴 |

## 핵심 개념

### Factory 인터페이스 계층

```
┌─────────────────────────────────────────────────────┐
│ Factory interface                                   │
│   CreateReader() (Reader, error)                    │
│   CreateWriter() (Writer, error)                    │
└──────────┬──────────────────────┬──────────────────┘
           │                      │
┌──────────▼────────┐  ┌──────────▼────────┐
│ MemoryFactory     │  │ FileFactory       │
│  (인메모리 저장)    │  │  (파일 기반 저장)  │
└───────────────────┘  └───────────────────┘
```

### Lazy Initialization (지연 초기화)

Jaeger ADR-003에서 제안한 패턴으로, 실제 Factory를 첫 접근 시점에 생성한다:

```
LazyFactory 생성 시: 실제 Factory = nil (초기화 안 됨)
첫 CreateReader() 호출 → 실제 Factory 생성 → 캐시
이후 CreateWriter() 호출 → 캐시된 Factory 반환
```

이 패턴의 이점:
- 사용하지 않는 백엔드는 초기화하지 않음
- 초기화 실패 시점을 실제 사용 시점으로 지연
- 시작 시간 단축

### Reader/Writer 분리

실제 Jaeger에서 Reader와 Writer는 별도 인터페이스로 정의되어 있지만,
구현체(Store)는 두 인터페이스를 동시에 구현한다. Factory는 동일한 Store 인스턴스를
Reader와 Writer 양쪽에 반환한다.

## 데모 시나리오

1. **MemoryFactory 테스트**: 인메모리 스토리지로 Span 쓰기/읽기
2. **FileFactory 테스트**: 파일 기반 스토리지로 동일한 인터페이스 테스트
3. **LazyFactory 테스트**: 지연 초기화 동작 확인
4. **Config 기반 전환**: 설정값만 변경하여 백엔드 전환
5. **에러 처리**: 지원하지 않는 백엔드 지정 시 에러

## 실행 방법

```bash
cd poc-storage-factory
go run main.go
```

## 출력 내용

1. **Factory 패턴 구조**: 인터페이스 계층 다이어그램
2. **Memory Factory**: 인메모리 스토리지 쓰기/읽기 결과
3. **File Factory**: 파일 기반 스토리지 쓰기/읽기 결과
4. **Lazy Factory**: 지연 초기화 동작 로그 (첫 접근 vs 캐시 반환)
5. **Config 전환**: 설정 기반 백엔드 전환 데모
6. **에러 처리**: 잘못된 백엔드 설정 시 에러 메시지

## Jaeger 소스코드와의 대응

| PoC 코드 | Jaeger 소스 |
|----------|------------|
| `Factory` 인터페이스 | `internal/storage/v2/api/tracestore/factory.go:Factory` |
| `Reader` 인터페이스 | `internal/storage/v2/api/tracestore/reader.go:Reader` |
| `Writer` 인터페이스 | `internal/storage/v2/api/tracestore/writer.go:Writer` |
| `MemoryFactory` | `internal/storage/v2/memory/factory.go:Factory` |
| `MemoryStore` | `internal/storage/v2/memory/memory.go:Store` |
| `FileFactory` | Badger Factory 개념 참고 (`internal/storage/v2/badger/factory.go`) |
| `LazyFactory` | ADR-003 지연 초기화 패턴 |
| `StorageConfig` | `internal/storage/v2/memory/config.go:Configuration` |
