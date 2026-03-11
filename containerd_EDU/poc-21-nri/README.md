# PoC-21: containerd NRI 플러그인 시뮬레이션

## 개요

NRI(Node Resource Interface)는 containerd/CRI-O에서 플러그인이 컨테이너 라이프사이클에
개입할 수 있는 표준 인터페이스이다. 이 PoC는 플러그인 체이닝, 컨테이너 스펙 수정을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 containerd 코드 | 시뮬레이션 |
|------|---------------------|-----------|
| NRI Plugin | `pkg/nri/` | 플러그인 인터페이스 + 체이닝 |
| Container Adjustment | NRI ContainerAdjustment | 환경변수/디바이스/리소스 수정 |
| CPU Pinning | 커스텀 NRI 플러그인 | QoS 기반 CPU 고정 할당 |
| Device Injection | 커스텀 NRI 플러그인 | GPU/RDMA 디바이스 주입 |
| Resource Limiter | 커스텀 NRI 플러그인 | 메모리 상한 클램핑 |

## 실행 방법

```bash
cd containerd_EDU/poc-21-nri
go run main.go
```
