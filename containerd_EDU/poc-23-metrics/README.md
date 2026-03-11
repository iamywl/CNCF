# PoC-23: containerd Cgroups 메트릭스 수집 시뮬레이션

## 개요

containerd는 cgroups v2를 통해 컨테이너의 CPU, 메모리, IO 메트릭스를 수집한다.
이 PoC는 메트릭스 수집, Prometheus 형식 변환, Pod 집계를 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 containerd 코드 | 시뮬레이션 |
|------|---------------------|-----------|
| cgroups v2 | `cgroups/` | CPU/Memory/IO 수집 |
| Prometheus | `metrics/` | 메트릭스 포매팅 |
| Pod Aggregation | CRI stats | 컨테이너별 → Pod 집계 |
| OOM Detection | cgroup memory events | OOM Kill 감지 |

## 실행 방법

```bash
cd containerd_EDU/poc-23-metrics
go run main.go
```
