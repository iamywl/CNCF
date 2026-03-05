# PoC-01: 쿠버네티스 아키텍처 — 허브 앤 스포크 패턴

## 개요

쿠버네티스의 핵심 아키텍처 원칙인 **허브 앤 스포크(Hub-and-Spoke)** 패턴을 시뮬레이션한다.
모든 컴포넌트(Controller, Scheduler, Kubelet)는 오직 API Server를 통해서만 통신하며,
서로 직접 통신하지 않는다.

## 구현 컴포넌트

| 컴포넌트 | 역할 | 실제 소스 위치 |
|---------|------|-------------|
| API Server | 중앙 허브, 상태 저장, Watch 전파 | `staging/src/k8s.io/apiserver/` |
| Scheduler | 미배정 파드 감시, 노드 배정 | `pkg/scheduler/scheduler.go` |
| Controller | 원하는 상태 ↔ 현재 상태 조정 | `pkg/controller/replicaset/replica_set.go` |
| Kubelet | 노드에 배정된 파드 실행 | `pkg/kubelet/kubelet.go` |

## 흐름

```
1. Controller: "web-server 3개 필요" → API Server에 파드 3개 생성
2. Scheduler: Watch로 미배정 파드 감지 → 노드 배정 후 API Server 업데이트
3. Kubelet: Watch로 자기 노드에 배정된 파드 감지 → 실행 후 상태 업데이트
```

## 핵심 포인트

- **Watch 메커니즘**: Go 채널로 변경사항을 실시간 전파
- **선언적 조정**: desired vs actual 비교로 상태 수렴
- **낙관적 동시성**: ResourceVersion으로 충돌 감지
- **관심사 분리**: 각 컴포넌트는 자기 역할만 수행

## 실행

```bash
go run main.go
```
