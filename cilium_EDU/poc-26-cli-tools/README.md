# PoC-26: Cilium CLI 도구 아키텍처 시뮬레이션

## 개요

Cilium CLI(cilium-cli)는 cobra 기반 계층적 커맨드 디스패치 구조를 사용한다.
이 PoC는 커맨드 트리 구조, 상태 진단, 연결성 테스트 등 핵심 CLI 기능을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 |
|------|------------------|-----------|
| Command Tree | cobra 기반 `cmd/` | 계층적 서브커맨드 등록/실행 |
| Status | `cilium-cli/status/` | 클러스터/노드 상태 진단 |
| Connectivity Test | `cilium-cli/connectivity/` | pod-to-pod 등 연결성 테스트 |
| Hubble Observe | `cilium-cli/hubble/` | 플로우 관찰 |

## 실행 방법

```bash
cd cilium_EDU/poc-26-cli-tools
go run main.go
```

## 핵심 포인트

- **커맨드 디스패치**: cobra처럼 트리 구조에서 서브커맨드를 탐색하여 실행
- **상태 진단**: 각 노드의 컴포넌트별 상태를 수집하여 표시
- **연결성 테스트**: pod-to-pod, pod-to-service 등 다양한 시나리오 테스트
