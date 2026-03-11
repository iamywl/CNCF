# PoC-24: containerd Checkpoint/Restore + Remote Snapshotter 시뮬레이션

## 개요

Checkpoint/Restore는 CRIU를 통해 실행 중인 컨테이너를 저장/복원하고,
Remote Snapshotter는 이미지 레이어를 lazy-pull하여 빠른 컨테이너 시작을 가능하게 한다.

## 시뮬레이션하는 개념

| 개념 | 실제 containerd 코드 | 시뮬레이션 |
|------|---------------------|-----------|
| CRIU Dump | `container_checkpoint.go` | 프로세스 메모리/레지스터/FD 저장 |
| CRIU Restore | checkpoint restore API | 상태 복원 |
| Remote Snapshotter | `snapshots/` | Lazy pull + on-demand fetch |
| Live Migration | checkpoint → transfer → restore | 마이그레이션 시나리오 |

## 실행 방법

```bash
cd containerd_EDU/poc-24-checkpoint-remote-snap
go run main.go
```
