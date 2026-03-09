# PoC-29: Pod Security Admission (PSA) 핵심 알고리즘 시뮬레이션

## 개요

Kubernetes Pod Security Admission의 핵심 알고리즘을 Go 표준 라이브러리만으로 재현한다.
실제 소스코드(`staging/src/k8s.io/pod-security-admission/`)의 설계를 그대로 따라 구현하였다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 재현하는 핵심 개념

### 1. Security Level 3단계

| 레벨 | 체크 수 | 소스 파일 |
|------|---------|----------|
| Privileged | 0개 (모두 허용) | `api/constants.go` |
| Baseline | 12개 (위험한 설정 차단) | `policy/check_*.go` (baseline) |
| Restricted | 7개 + Baseline 전체 | `policy/check_*.go` (restricted) |

### 2. Check Registry와 버전 인플레이션

`policy/registry.go`의 `checkRegistry` 구조를 재현한다.

- 각 체크는 `MinimumVersion`을 가지며, 해당 버전부터 다음 체크 버전까지 동일한 함수를 적용
- 예: `sysctls` 체크는 v1.0, v1.27에서 허용 목록이 다름 -> 버전별로 다른 함수 적용
- `NewCheckRegistry()`에서 `inflateVersions()`를 수행하여 모든 마이너 버전에 대해 사전 계산

### 3. 오버라이드 시스템

Restricted 체크가 Baseline 체크를 대체하는 메커니즘을 재현한다.

- `capabilities_restricted`가 `capabilities_baseline`을 오버라이드
- `restrictedVolumes`가 `hostPathVolumes`를 오버라이드
- `seccompProfile_restricted`가 `seccompProfile_baseline`을 오버라이드
- 중복 에러 메시지 방지 및 더 엄격한 검사로 대체

### 4. Enforce / Audit / Warn 3-Mode 평가

`admission/admission.go`의 `EvaluatePod()` 로직을 재현한다.

- **Enforce**: 위반 시 요청 거부 (403 Forbidden)
- **Audit**: 위반 시 감사 로그에 annotation 기록
- **Warn**: 위반 시 API 응답에 Warning 헤더 추가
- 이미 거부된 요청에는 Warn 경고를 추가하지 않음

### 5. 결과 캐싱

동일한 `LevelVersion`이면 평가를 재사용한다. 예: Enforce와 Audit이 모두 `baseline:v1.28`이면 Evaluator를 한 번만 호출한다.

### 6. Exemption 시스템

세 가지 면제 메커니즘을 재현한다.

- **Namespace 면제**: `kube-system` 등 시스템 네임스페이스
- **User 면제**: 시스템 서비스 계정
- **RuntimeClass 면제**: `gvisor`, `kata-containers` 등 자체 격리 제공 런타임

### 7. Namespace 라벨 파싱

`pod-security.kubernetes.io/*` 라벨에서 Policy를 파싱하는 로직을 재현한다.

## 시나리오 목록

| # | 시나리오 | 검증 내용 |
|---|---------|----------|
| 1 | Security Level별 평가 | Privileged는 모든 허용, Baseline은 특권 컨테이너 거부 |
| 2 | Baseline vs Restricted | allowPrivilegeEscalation이 Baseline 통과, Restricted 거부 |
| 3 | Enforce/Audit/Warn 분리 | Enforce=privileged에서 허용되지만 Audit/Warn에 위반 기록 |
| 4 | 버전별 체크 차이 | v1.25에서 거부되는 sysctl이 v1.27에서 허용 |
| 5 | Exemption 시스템 | Namespace, User, RuntimeClass 면제 동작 확인 |
| 6 | 오버라이드 시스템 | Baseline에서 통과하는 capability가 Restricted에서 거부 |
| 7 | Restricted 완전 통과 | 모든 보안 설정을 갖춘 Pod가 Restricted 통과 |
| 8 | Namespace 라벨 파싱 | 라벨에서 Policy 객체 생성 및 평가 |
| 9 | 결과 캐싱 | 동일 LevelVersion에서 캐시 재사용 확인 |

## 대응하는 실제 소스 파일

| PoC 구현 | 실제 소스 파일 |
|----------|--------------|
| `Level`, `Version`, `Policy` 타입 | `api/constants.go`, `api/helpers.go` |
| `Check`, `VersionedCheck`, `CheckResult` 타입 | `policy/checks.go` |
| `CheckRegistry`, `NewCheckRegistry()` | `policy/registry.go` |
| `checkPrivileged()` 등 14개 체크 | `policy/check_*.go` (19개 중 주요 14개) |
| `Admission.Evaluate()` | `admission/admission.go` `EvaluatePod()` |
| `ParsePolicyFromLabels()` | `admission/admission.go` `PolicyToEvaluate()` |
| `ExemptionConfig` | `admission/api/types.go` `PodSecurityConfiguration.Exemptions` |
