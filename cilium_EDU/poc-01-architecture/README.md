# PoC-01: Cilium Hive Cell 아키텍처 시뮬레이션

## 개요

Cilium은 **Hive**라는 자체 의존성 주입(DI) 프레임워크로 에이전트 내부 컴포넌트를 관리한다.
이 PoC는 Hive의 Cell 아키텍처를 시뮬레이션하여 핵심 개념을 이해한다.

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 |
|------|------------------|-----------|
| Cell | `pkg/hive/cell/cell.go` | Cell 인터페이스 (Name, Start, Stop) |
| Hive | `pkg/hive/hive.go` | Hive 컨테이너 (등록, 시작, 정지) |
| Lifecycle Hook | `pkg/hive/lifecycle.go` | HookContext를 받는 Start/Stop 메서드 |
| Config 바인딩 | `pkg/hive/cell/config.go` | Config 구조체로 설정값 주입 |
| Cell 레벨 | 계층 구조 | Infrastructure → ControlPlane → Datapath |

## 동작 흐름

```
1. Config 로드 (cilium-config ConfigMap 시뮬레이션)
2. Cell 등록 (K8sClient, APIServer, EndpointManager, PolicyEngine, BPFLoader, MapManager)
3. 의존성 검증 (모든 Cell의 Dependencies가 충족되는지 확인)
4. 레벨 순 정렬 (Infrastructure → ControlPlane → Datapath)
5. 순서대로 Start 호출
6. 시그널 대기 (SIGINT/SIGTERM 또는 2초 타이머)
7. 역순으로 Stop 호출 (Datapath → ControlPlane → Infrastructure)
```

## 실행 방법

```bash
cd cilium_EDU/poc-01-architecture
go run main.go
```

## 핵심 포인트

- **계층 구조**: Infrastructure가 먼저 시작되어야 ControlPlane이 동작하고,
  ControlPlane이 준비되어야 Datapath가 BPF 프로그램을 로딩할 수 있다.
- **역순 종료**: 종료 시에는 Datapath부터 먼저 정리하여 트래픽 유실을 최소화한다.
- **의존성 검증**: 시작 전에 모든 Cell의 의존성이 충족되는지 확인한다.
- **Graceful Shutdown**: SIGTERM 수신 시 모든 Cell을 안전하게 정리한다.
