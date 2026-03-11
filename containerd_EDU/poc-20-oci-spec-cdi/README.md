# PoC-20: containerd OCI Spec + CDI 디바이스 주입 시뮬레이션

## 개요

containerd는 컨테이너 실행 시 OCI 런타임 스펙을 생성하고, CDI를 통해 GPU 등 디바이스를 주입한다.
이 PoC는 스펙 생성(SpecOpts 패턴), CDI 레지스트리, 디바이스 주입을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 containerd 코드 | 시뮬레이션 |
|------|---------------------|-----------|
| OCI Spec | `oci/spec.go` | 런타임 스펙 구조체 생성 |
| SpecOpts | `oci/spec_opts.go` | 함수형 옵션으로 스펙 수정 |
| CDI | `pkg/cdi/` | 벤더 독립적 디바이스 주입 |
| Device Injection | CDI ContainerEdits | 디바이스/마운트/환경변수/훅 주입 |

## 실행 방법

```bash
cd containerd_EDU/poc-20-oci-spec-cdi
go run main.go
```
