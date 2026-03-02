# PoC: Hubble Peer 디스커버리 & 관리 패턴

## 관련 문서
- [02-ARCHITECTURE.md](../02-ARCHITECTURE.md) - Relay 아키텍처
- [04-SEQUENCE-DIAGRAMS.md](../04-SEQUENCE-DIAGRAMS.md) - Relay 멀티노드 시퀀스

## 개요

Hubble Relay는 클러스터의 모든 Hubble Server(노드)를 동적으로 발견하고 관리합니다:
- **gRPC 스트리밍**: Peer 서비스로 실시간 노드 변경 알림
- **변경 알림**: PEER_ADDED / PEER_UPDATED / PEER_DELETED
- **연결 상태**: IDLE → CONNECTING → READY / TRANSIENT_FAILURE
- **지수 백오프**: 연결 실패 시 점진적 재시도

## 실행

```bash
go run main.go
```

## 시나리오

### 시나리오 1: 초기 Peer 발견
3개 노드 동시 발견 및 등록

### 시나리오 2: 연결 상태 변화
CONNECTING → READY (성공) / TRANSIENT_FAILURE (실패)

### 시나리오 3: 동적 변경
새 노드 추가 (스케일 업) + 노드 삭제 (스케일 다운)

### 시나리오 4: 지수 백오프
연결 실패 시 100ms → 200ms → 400ms → ... → 5s(max) 대기

## 핵심 학습 내용
- gRPC 스트리밍 기반 실시간 서비스 디스커버리
- 동시성 안전한 피어 풀 관리 (`sync.RWMutex`)
- 연결 상태 머신 (gRPC connectivity states)
- 지수 백오프 + 지터 패턴
