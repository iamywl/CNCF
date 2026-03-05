# PoC-09: Admission Controller 체인

## 개요

Kubernetes API 서버의 Admission Controller 체인을 시뮬레이션한다.

Admission Controller는 API 요청이 인증/인가를 통과한 후, etcd에 저장되기 전에 실행되는 플러그인 체인이다. Mutating Admission이 리소스를 수정하고, Validating Admission이 최종 결과를 검증한다.

## 실제 코드 참조

| 파일 | 역할 |
|------|------|
| `staging/src/k8s.io/apiserver/pkg/admission/interfaces.go` | Interface, MutationInterface, ValidationInterface |
| `staging/src/k8s.io/apiserver/pkg/admission/chain.go` | chainAdmissionHandler (체인 실행 로직) |

## 시뮬레이션하는 개념

### 1. Admission 인터페이스
- `Handles(Operation) bool`: 이 플러그인이 해당 연산을 처리하는지
- `Admit(attrs)`: 리소스를 검사하고 변경 (Mutating)
- `Validate(attrs)`: 리소스를 검증만 함 (Validating)

### 2. 체인 실행 순서
- Phase 1: 모든 MutatingPlugin의 Admit() 실행
- Phase 2: 모든 ValidatingPlugin의 Validate() 실행
- 에러 발생 시 즉시 중단하고 요청 거부

### 3. 빌트인 플러그인
- **NamespaceLifecycle**: 삭제 중인 네임스페이스에 리소스 생성 차단
- **ServiceAccount**: Pod에 기본 ServiceAccount 주입
- **LimitRanger**: CPU/메모리 기본값 주입 + 최대값 검증
- **PodSecurity**: privileged/hostNetwork 등 보안 정책 검증
- **DefaultTolerationSeconds**: 기본 Toleration 주입

### 4. Webhook Admission
- 외부 HTTP 서버에 AdmissionReview를 전송하여 검증 위임
- 실제 ValidatingWebhookConfiguration에 대응

## 실행

```bash
go run main.go
```
