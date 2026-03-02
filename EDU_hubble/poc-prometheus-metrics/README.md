# PoC: Hubble Prometheus 메트릭 패턴

## 관련 문서
- [06-OPERATIONS.md](../06-OPERATIONS.md) - 모니터링 및 메트릭
- [07-CODE-GUIDE.md](../07-CODE-GUIDE.md) - 메트릭 시스템 구현

## 개요

Hubble은 Prometheus를 사용하여 네트워크 관측 메트릭을 수집합니다:
- **Counter**: 총 Flow 수, 드롭된 Flow 수 등 단조 증가 값
- **Gauge**: 연결된 Peer 수, 현재 Ring Buffer 사용률 등 현재 값
- **Histogram**: gRPC 요청 지연시간 등 분포 데이터

## 실행

```bash
go run main.go
```

## 시나리오

### 시나리오 1: Counter (Flow 처리 카운트)
10개 Flow를 처리하며 FORWARDED/DROPPED를 분류

### 시나리오 2: Gauge (Peer 연결 상태)
READY/CONNECTING/IDLE 상태 변화 추적

### 시나리오 3: Histogram (요청 지연시간)
gRPC 요청 지연시간을 버킷별로 분류하여 분포 확인

### /metrics 출력
Prometheus 텍스트 형식의 전체 메트릭 출력

## 핵심 학습 내용
- Prometheus 3대 메트릭 타입 (Counter, Gauge, Histogram)
- 레이블을 통한 메트릭 차원 분리
- Registry 패턴으로 메트릭 중앙 관리
- `/metrics` 엔드포인트의 텍스트 형식 이해
