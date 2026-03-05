# PoC 18: Kubernetes 코드 생성 패턴

## 개요

Kubernetes는 반복적인 boilerplate 코드를 code-generator 도구로 자동 생성한다.
이 PoC는 핵심 코드 생성 패턴을 Go 표준 라이브러리만으로 시뮬레이션한다.

## 시뮬레이션하는 패턴

| 패턴 | 실제 도구 | 생성 파일 | PoC 구현 |
|------|----------|----------|----------|
| DeepCopy | deepcopy-gen | `zz_generated.deepcopy.go` | `DeepCopy()`, `DeepCopyInto()` |
| Scheme 등록 | register-gen | `zz_generated.register.go` | `Scheme.AddKnownType()` |
| 버전 변환 | conversion-gen | `zz_generated.conversion.go` | `Scheme.Convert()` |
| Defaulting | defaulter-gen | `zz_generated.defaults.go` | `Scheme.Default()` |
| 타입별 클라이언트 | client-gen | `typed/core/v1/pod.go` | `PodInterface`, `Clientset` |

## 핵심 개념: 변환 허브 패턴

```
v1alpha1 ──→ internal ──→ v1
v1alpha1 ←── internal ←── v1
```

- Internal 타입이 허브(hub) 역할
- N개 버전 → 2N개 변환 함수 (N²가 아님)
- 각 외부 버전은 internal과만 변환하면 됨

## 실행

```bash
go run main.go
```

## 소스코드 참조

- Scheme: `staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go`
- code-generator: `staging/src/k8s.io/code-generator/cmd/`
- 생성 스크립트: `hack/update-codegen.sh`
