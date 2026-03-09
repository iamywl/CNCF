# PoC: Hubble Field Mask 필터링 시뮬레이션

## 개요

Hubble의 Field Mask 필터링 시스템을 Go 표준 라이브러리만으로 시뮬레이션한다.
Field Mask는 GetFlows API에서 관찰자가 필요한 필드만 선택적으로 수신하여
네트워크 대역폭과 처리 비용을 절감하는 핵심 최적화 메커니즘이다.

## 대응하는 Hubble 소스코드

| 이 PoC | Hubble 소스 | 설명 |
|--------|------------|------|
| `Flow` (map) | `api/v1/observer/observer.proto` | Flow 메시지 구조 |
| `PathTree` | `google.golang.org/protobuf/types/known/fieldmaskpb` | FieldMask 경로 트리 |
| `ApplyFieldMask()` | `hubble/cmd/observe/flows.go` | 필드 마스크 적용 로직 |
| `DefaultFieldMask` | `hubble/pkg/defaults/defaults.go` | 기본 FieldMask 경로 19개 |
| `ValidatePath()` | `observer.proto` 스키마 기반 | 경로 유효성 검증 |
| 출력 형식 호환성 | `hubble/cmd/observe/observe.go` | compact/dict/tab 형식 제한 |
| 마이그레이션 | `observer.proto` experimental → 최상위 | FieldMask 필드 이전 |

## 구현 내용

### 1. 경로 트리 (PathTree)
- 점(`.`) 구분 경로를 트리 구조로 변환
- 부모 노드 선택 시 모든 하위 필드 포함
- 리프 노드까지 세밀한 필드 선택

### 2. 필드 마스크 필터링
- 중첩된 map 구조에서 지정 경로만 재귀적 추출
- 존재하지 않는 경로는 무시 (protobuf 호환)

### 3. 기본 FieldMask (19개 경로)
- `time`, `source.*`, `destination.*`, `l4`, `IP`, `verdict` 등
- Hubble CLI의 기본 출력에 필요한 최소 필드셋

### 4. 경로 검증
- Flow 메시지 스키마 기반 경로 유효성 확인
- 존재하지 않는 필드 경로 사전 차단

### 5. 대역폭 절감 분석
- 전체 Flow: ~1300 bytes
- 기본 마스크: ~716 bytes (45% 절감)
- 최소 마스크: ~107 bytes (92% 절감)

### 6. 출력 형식 호환성
- json/jsonpb: 커스텀 마스크 호환
- compact/dict/tab: 기본 마스크만 호환

### 7. Experimental → 최상위 마이그레이션
- 기존 `req.Experimental.FieldMask` → 신규 `req.FieldMask`
- 서버: 최상위 우선, 없으면 Experimental fallback

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- Field Mask는 gRPC 스트리밍에서 대역폭을 최대 92%까지 절감한다
- PathTree 구조로 중첩 경로를 효율적으로 매칭한다
- 기본 FieldMask 19개 경로는 CLI 출력의 모든 형식을 지원하는 최소 필드셋이다
- 커스텀 마스크 사용 시 compact/dict/tab 형식은 사용할 수 없다
