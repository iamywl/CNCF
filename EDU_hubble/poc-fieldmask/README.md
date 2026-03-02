# PoC: Hubble Field Mask 최적화 패턴

## 관련 문서
- [05-API-REFERENCE.md](../05-API-REFERENCE.md) - GetFlows API
- [07-CODE-GUIDE.md](../07-CODE-GUIDE.md) - 파서 최적화

## 개요

Field Mask는 클라이언트가 필요한 필드만 요청하여 성능을 최적화하는 Protobuf 표준 패턴입니다:
- **네트워크 절약**: 불필요한 필드 전송 없음
- **CPU 절약**: 불필요한 필드 계산 스킵
- **트리 구조**: `source.pod_name` → `{ source: { pod_name: leaf } }`

## 실행

```bash
go run main.go
```

## 시나리오

### 시나리오 0: 전체 Flow
모든 필드가 채워진 원본 Flow (기준값)

### 시나리오 1: 간단한 모니터링
verdict + pod_name만 요청 → 대폭 절약

### 시나리오 2: 네트워크 분석
L4 + namespace + verdict → 중간 수준

### 시나리오 3: L7 HTTP 분석
HTTP method/status/URL만 요청 → API 모니터링 최적화

## 핵심 학습 내용
- `strings.Cut`으로 점 구분 경로를 재귀적 트리로 변환
- 선택적 필드 복사 패턴 (copy-on-read)
- `google.protobuf.FieldMask` 표준 이해
- 대규모 스트리밍에서 50-80% 대역폭 절약 효과
