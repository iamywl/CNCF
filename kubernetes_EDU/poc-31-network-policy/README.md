# PoC-31: NetworkPolicy 핵심 알고리즘 시뮬레이션

## 개요

Kubernetes NetworkPolicy의 핵심 동작 원리를 Go 표준 라이브러리만으로 시뮬레이션한다.
실제 Kubernetes 소스코드(`staging/src/k8s.io/api/networking/v1/types.go`,
`pkg/apis/networking/validation/validation.go`)의 타입 시스템과 검증 로직을 재현하여,
NetworkPolicy가 트래픽을 어떻게 매칭하고 허용/차단을 결정하는지 보여준다.

## 실행 방법

```bash
go run main.go
```

## 구현 내용

### 1. NetworkPolicy 타입 시스템
- `NetworkPolicy`, `NetworkPolicySpec`, `NetworkPolicyIngressRule`, `NetworkPolicyEgressRule`
- `NetworkPolicyPort` (Protocol, Port, EndPort)
- `NetworkPolicyPeer` (PodSelector, NamespaceSelector, IPBlock)
- `IPBlock` (CIDR, Except)
- `PolicyType` (Ingress, Egress)

### 2. Pod/Namespace 셀렉터 매칭
- 빈 셀렉터 `{}`는 모든 대상을 선택
- `nil` 셀렉터는 "미지정"으로 매칭하지 않음
- PodSelector만 지정: 같은 네임스페이스 내 Pod 매칭
- NamespaceSelector만 지정: 해당 네임스페이스의 모든 Pod 매칭
- PodSelector + NamespaceSelector: AND 관계 (특정 NS의 특정 Pod)

### 3. IPBlock CIDR 매칭
- `net.ParseCIDR`을 사용한 CIDR 범위 확인
- Except 처리: CIDR에 포함되지만 Except에도 포함되면 매칭 실패

### 4. 포트 범위 매칭
- 단일 포트 매칭
- Port ~ EndPort 범위 매칭
- Protocol 기반 필터링 (TCP/UDP/SCTP)
- Port 미지정 시 모든 포트 허용

### 5. 정책 합산 (Additive)
- 같은 Pod에 적용되는 여러 정책은 OR로 합산
- 어떤 정책에서든 허용된 트래픽은 허용
- 어떤 정책도 다른 정책의 허용을 취소할 수 없음

### 6. 트래픽 판정 엔진
- 대상 Pod에 적용되는 정책 수집
- PolicyTypes 기반 방향 필터링
- 규칙 매칭: From/To (OR) AND Ports (OR)
- 정책 없으면 기본 허용, 정책 있으면 기본 차단

### 7. 검증 로직
- Port/EndPort 조합 검증
- IPBlock과 PodSelector/NamespaceSelector 배타성 검증
- Except가 CIDR의 엄격한 부분집합인지 검증

## 시뮬레이션 시나리오

| 시나리오 | 설명 | 핵심 개념 |
|---------|------|----------|
| 1 | 정책 없음 | 기본 허용 (모든 트래픽 통과) |
| 2 | Default Deny Ingress | 빈 Ingress 규칙 = 모든 인바운드 차단 |
| 3 | Additive 정책 | 여러 정책의 OR 합산 동작 |
| 4 | NamespaceSelector | 다른 네임스페이스에서의 접근 제어 |
| 5 | IPBlock + Except | CIDR 매칭과 예외 처리 |
| 6 | 포트 범위 | Port ~ EndPort 범위 매칭 |
| 7 | Egress 정책 | 아웃바운드 트래픽 제어 |
| 8 | 검증 로직 | 유효/무효 설정 검증 |

## 참조 소스코드

| 파일 | 역할 |
|------|------|
| `staging/src/k8s.io/api/networking/v1/types.go` | API 타입 정의 |
| `pkg/apis/networking/validation/validation.go` | 검증 로직 |
| `pkg/registry/networking/networkpolicy/strategy.go` | 레지스트리 전략 |
