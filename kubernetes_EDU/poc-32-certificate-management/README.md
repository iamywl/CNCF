# PoC 32: Kubernetes 인증서 관리 및 Bootstrap 인증

## 개요

Kubernetes 인증서 관리 시스템의 핵심 알고리즘을 Go 표준 라이브러리만으로 시뮬레이션한다.
CSR 생성/승인/서명 흐름, Bootstrap Token 인증, 인증서 로테이션, Signer Name 기반 라우팅까지
전체 인증서 라이프사이클을 재현한다.

## 실행 방법

```bash
go run main.go
```

## 시뮬레이션 항목

### 1. CSR 승인 흐름 (sarApprover 패턴)

`pkg/controller/certificates/approver/sarapprove.go`의 핵심 패턴을 재현한다.

- `csrRecognizer` 패턴: 인식 -> 권한 확인 -> 승인 3단계 파이프라인
- `isSelfNodeClientCert` / `isNodeClientCert` 인식기
- `SubjectAccessReview` 기반 권한 확인 시뮬레이션
- 승인/미승인/미인식 케이스 처리

### 2. CSR 서명/발급 (CA Signer)

`pkg/controller/certificates/signer/signer.go`의 서명 로직을 재현한다.

- 실제 ECDSA P-256 CA 인증서 생성
- `x509.CreateCertificate`로 인증서 서명
- `duration()` 함수의 유효 기간 결정 로직 (상한/하한/기본값)
- 5분 backdate 적용 (시계 오차 보정)
- 서명된 인증서의 상세 정보 검증

### 3. Bootstrap Token 인증

`plugin/pkg/auth/authenticator/token/bootstrap/bootstrap.go`의 인증 흐름을 재현한다.

- `<token-id>.<token-secret>` 형식 파싱 (6자리 + 16자리)
- `subtle.ConstantTimeCompare`로 타이밍 공격 방지
- 만료 시간 검증
- 인증 용도(usage-bootstrap-authentication) 확인
- 삭제 대기 Secret 거부
- 추가 그룹(auth-extra-groups) 처리
- `system:bootstrap:<id>` 사용자명 생성

### 4. 인증서 로테이션

`staging/src/k8s.io/client-go/transport/cert_rotation.go`의 동적 인증서 교체를 재현한다.

- `RWMutex` 기반 동시성 안전한 인증서 캐싱
- 변경 감지: RLock으로 읽기 비교 -> 변경 시에만 Lock으로 쓰기
- 최초 로드 vs 로테이션 구분 (최초 로드 시 연결 끊지 않음)
- `CloseAll()` 연결 재설정 시뮬레이션

### 5. Signer Name 기반 라우팅

`pkg/controller/certificates/signer/signer.go`의 signer별 독립 처리를 재현한다.

- 4개 Signer Name별 독립 인스턴스 생성
- `getCSRVerificationFuncForSignerName()` 패턴
- signer별 검증 함수: `isKubeletServing`, `isKubeletClient`, `isKubeAPIServerClient`, `isLegacyUnknown`
- `client auth` / `server auth` usage 검증
- 검증 실패 시 `Failed` 조건 추가

### 6. 결정론적 CSR 이름 생성

`pkg/kubelet/certificate/bootstrap/bootstrap.go`의 `digestedName()` 함수를 재현한다.

- SHA-256 해시 기반 결정론적 이름 생성
- base64 인코딩 + 구분자(`|`)로 충돌 방지
- 동일 입력에 대한 멱등성(idempotency) 보장
- 서로 다른 입력에 대한 고유성 보장

## 참조 소스 코드

| 구성요소 | 소스 위치 |
|---------|----------|
| CSR 타입 정의 | `staging/src/k8s.io/api/certificates/v1/types.go` |
| CSR 승인기 | `pkg/controller/certificates/approver/sarapprove.go` |
| CSR 서명기 | `pkg/controller/certificates/signer/signer.go` |
| CA Provider | `pkg/controller/certificates/signer/ca_provider.go` |
| Bootstrap Token 인증 | `plugin/pkg/auth/authenticator/token/bootstrap/bootstrap.go` |
| Kubelet Bootstrap | `pkg/kubelet/certificate/bootstrap/bootstrap.go` |
| 인증서 로테이션 | `staging/src/k8s.io/client-go/transport/cert_rotation.go` |
| Bootstrap Token API | `staging/src/k8s.io/cluster-bootstrap/token/api/types.go` |

## 핵심 설계 패턴

| 패턴 | 설명 | 구현 위치 |
|------|------|----------|
| Recognizer 체인 | CSR 유형 인식 -> SAR 권한 확인 -> 승인 | `sarApprover.handle()` |
| 플러그인 핸들러 | `handler func` 주입으로 승인/서명 교체 | `CertificateController` |
| Signer별 격리 | 각 Signer Name이 독립 컨트롤러 인스턴스 | `NewCSRSigningController()` |
| Constant-time 비교 | 타이밍 공격 방지 | `TokenAuthenticator` |
| 결정론적 이름 | 재시도 안전성, 중복 방지 | `digestedName()` |
| 동적 인증서 교체 | RWMutex + 변경 감지 + 연결 재설정 | `dynamicClientCert` |
