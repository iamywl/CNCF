# PoC: Hubble Graceful Shutdown 패턴

## 관련 문서
- [02-ARCHITECTURE.md](../02-ARCHITECTURE.md) - Relay 서버 라이프사이클
- [06-OPERATIONS.md](../06-OPERATIONS.md) - 운영 및 프로세스 관리

## 개요

Hubble은 OS 시그널(SIGINT/SIGTERM) 수신 시 다음 순서로 정리합니다:
1. Context 취소 → 모든 goroutine에 종료 신호 전파
2. errgroup.Wait()로 워커 goroutine 종료 대기
3. 리소스 정리 (시작의 역순): Peer → gRPC → TLS

## 실행

```bash
go run main.go
```

3초 후 자동 종료되거나, Ctrl+C로 수동 중단 가능

## 시나리오

### 자동 종료 (3초 타임아웃)
워커가 Flow를 수집하다가 자동으로 Graceful Shutdown 실행

### 수동 중단 (Ctrl+C)
SIGINT 시그널로 즉시 Graceful Shutdown 시작

## 핵심 학습 내용
- `os/signal.Notify`로 OS 시그널 채널 변환
- `context.WithCancel`로 goroutine 트리 전체에 취소 전파
- `errgroup` 패턴으로 동시 goroutine 관리
- 리소스 정리 역순 패턴 (LIFO)
- `select` 문으로 다중 이벤트 소스 처리
