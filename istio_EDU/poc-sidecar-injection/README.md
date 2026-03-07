# PoC 6: 사이드카 인젝션 (Sidecar Injection)

## 개요

Istio의 사이드카 인젝션 메커니즘을 시뮬레이션합니다. Kubernetes MutatingWebhook을 통해
Pod 생성 시 자동으로 Envoy 사이드카 프록시를 주입하는 과정을 재현합니다.

실제 코드 경로:
- `istio/pkg/kube/inject/inject.go` (정책 평가, 템플릿 렌더링)
- `istio/pkg/kube/inject/webhook.go` (Webhook 처리, JSON Patch 생성)

## 핵심 로직

### 1. injectRequired() - 인젝션 정책 평가

우선순위 (높은 순):
1. `hostNetwork == true` → 항상 스킵
2. 무시 네임스페이스 (kube-system 등) → 항상 스킵
3. Pod 레이블 `sidecar.istio.io/inject` → true/false
4. Pod 어노테이션 `sidecar.istio.io/inject` → true/false (레이블이 우선)
5. `NeverInjectSelector` 매칭 → false
6. `AlwaysInjectSelector` 매칭 → true
7. 네임스페이스 정책 (`config.Policy`) → enabled면 true, disabled면 false

### 2. RunTemplate() - 사이드카 템플릿 렌더링

- `istio-init` 컨테이너: iptables 규칙 설정 (트래픽 리다이렉션)
- `istio-proxy` 컨테이너: Envoy 사이드카 프록시
- 볼륨: istio-envoy, istiod-ca-cert, istio-podinfo 등

### 3. JSON Patch 생성

원본 Pod와 수정된 Pod를 비교하여 RFC 6902 JSON Patch를 생성하고
AdmissionResponse로 반환합니다.

## 시뮬레이션 내용

1. **인젝션 정책 평가**: 다양한 시나리오에서 인젝션 여부 결정
2. **사이드카 인젝션 실행**: 실제 Pod에 사이드카 추가 (before/after)
3. **JSON Patch 생성**: 원본 대비 변경 사항 패치 생성
4. **인젝션 흐름 시각화**: 전체 프로세스 다이어그램

## 실행

```bash
go run main.go
```

## 주요 참조 코드

| 함수 | 파일 위치 | 설명 |
|------|----------|------|
| `InjectionPolicy` | `inject.go:59` | 인젝션 정책 타입 |
| `injectRequired()` | `inject.go:199` | 인젝션 필요 여부 판단 |
| `RunTemplate()` | `inject.go:430` | 사이드카 템플릿 렌더링 |
| `SidecarInjectionStatus` | `inject.go:897` | 인젝션 결과 상태 |
| `Config` | `inject.go:134` | 인젝션 설정 구조체 |
