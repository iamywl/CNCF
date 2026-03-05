# PoC-11: Flow 익스포터 파이프라인

## 개요

Hubble의 Flow 익스포트 파이프라인을 시뮬레이션한다. 이벤트가 필터(AllowList/DenyList)를 통과하고, OnExportEvent 훅을 거친 뒤, FieldMask로 필요한 필드만 선택하여 JSON으로 인코딩하고 파일에 쓰는 전체 과정을 재현한다.

파일 로테이션(lumberjack 기반), 다중 익스포터 구성, 커스텀 훅 삽입 등 실제 운영 환경의 패턴을 포함한다.

## 핵심 개념

### 1. Export 파이프라인

```
Event 수신
    │
    ▼
ctx.Done 확인
    │
    ▼
AllowList/DenyList 필터
    │
    ▼
OnExportEvent 훅
    │
    ▼
Event → ExportEvent 변환
    │
    ▼
FieldMask 적용
    │
    ▼
Encoder.Encode (JSON)
    │
    ▼
Writer (파일/stdout)
```

### 2. 필터 시스템

- **AllowList**: 하나라도 매치되면 허용 (빈 리스트는 전체 허용)
- **DenyList**: 하나라도 매치되면 거부 (DenyList가 AllowList보다 우선)

### 3. FieldMask

특정 필드만 익스포트하도록 마스킹한다. 네트워크 대역폭과 저장 공간을 절약할 수 있다.

### 4. 파일 로테이션

`RotatingWriter`가 파일 크기를 감시하여 `maxSize` 초과 시 자동으로 백업 파일을 생성하고 새 파일을 연다. 오래된 백업은 `maxBackups` 수를 초과하면 자동 삭제된다.

### 5. OnExportEvent 훅

익스포트 파이프라인에 커스텀 로직을 삽입할 수 있다. 훅이 `stop=true`를 반환하면 해당 이벤트의 익스포트를 중단한다.

## 실행 방법

```bash
go run main.go
```

6가지 테스트를 실행한다: 기본 JSON 익스포트, 필터 적용, FieldMask 적용, OnExportEvent 훅, 파일 로테이션, DenyList 필터.

## 실제 소스코드 참조

| 파일 | 핵심 함수/구조체 |
|------|-----------------|
| `cilium/pkg/hubble/exporter/exporter.go` | `exporter.Export()` - 익스포트 파이프라인 |
| `cilium/pkg/hubble/exporter/encoder.go` | `Encoder` 인터페이스, `JsonEncoder` |
| `cilium/pkg/hubble/exporter/writer.go` | `FileWriter` - 파일 쓰기 + 로테이션 |
| `cilium/pkg/hubble/exporter/option.go` | `Options` - AllowList, DenyList, FieldMask |
| `cilium/pkg/hubble/exporter/exporteroption/option.go` | `OnExportEvent` - 훅 인터페이스 |
