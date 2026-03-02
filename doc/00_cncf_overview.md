# CNCF 개요 및 등급 체계

## CNCF란?

CNCF(Cloud Native Computing Foundation)는 Linux Foundation 산하 재단으로, 클라우드 네이티브 오픈소스 프로젝트의 육성과 관리를 담당한다.

Kubernetes, Prometheus, Envoy 등 클라우드 네이티브 생태계의 핵심 프로젝트들이 CNCF에 속해 있다.

## 등급 체계 (Maturity Levels)

CNCF는 프로젝트를 성숙도에 따라 3단계로 분류한다:

```
Sandbox → Incubating → Graduated
(실험)     (성장)        (성숙)
```

### Sandbox (실험 단계)

- 초기 단계 프로젝트. 가능성은 있으나 프로덕션 사용은 이름
- CNCF TOC(Technical Oversight Committee) 스폰서 1명 이상 필요
- 연 1회 리뷰를 통해 지속 여부 결정
- 예: DevSpace, ko, Porter, Score

### Incubating (성장 단계)

- 실제 프로덕션 사용 사례가 존재하는 프로젝트
- 요구사항:
  - 최소 3개 이상의 독립적 프로덕션 사용자
  - 건강한 기여자(contributor) 생태계
  - 보안 감사(security audit) 통과
  - 명확한 거버넌스 구조
- 예: Backstage, KubeVirt, NATS, Chaos Mesh

### Graduated (성숙 단계)

- 완전히 성숙한 프로젝트. 업계 표준 수준
- 요구사항:
  - 광범위한 프로덕션 채택
  - 거버넌스 확립 및 문서화
  - 보안 감사 완료
  - 장기 커미터(committer) 다수
  - CNCF 행동 강령 준수
- **"프로덕션에 안심하고 써도 된다"** 는 CNCF의 보증과 같음
- 예: Kubernetes, Helm, Prometheus, Argo, Flux

### Archived (보관)

- 더 이상 활발히 유지보수되지 않는 프로젝트
- 후속 프로젝트로 대체되었거나 커뮤니티가 비활성화됨
- 예: Brigade, Keptn, Nocalhost, Pravega

## CNCF Landscape 카테고리

| 대분류 | 소분류 |
|---|---|
| **App Definition & Development** | Application Definition & Image Build |
| | Continuous Integration & Delivery |
| | Database |
| | Streaming & Messaging |
| **Observability & Analysis** | Chaos Engineering |
| | Continuous Optimization |
| | Logging |
| | Monitoring |
| | Tracing |

이 외에도 Orchestration & Management, Runtime, Provisioning 등의 카테고리가 있으나, 이 문서에서는 위 카테고리를 중심으로 정리한다.

## Graduated 프로젝트 전체 목록 (주요)

| 프로젝트 | 카테고리 | 핵심 역할 |
|---|---|---|
| Kubernetes | Orchestration | 컨테이너 오케스트레이션 |
| Prometheus | Monitoring | 메트릭 수집/쿼리 |
| Envoy | Service Mesh | L7 프록시/서비스 메시 데이터 플레인 |
| CoreDNS | Runtime | K8s 클러스터 DNS |
| containerd | Runtime | 컨테이너 런타임 |
| Fluentd | Logging | 로그 수집 파이프라인 |
| Fluent Bit | Logging | 경량 로그 수집 |
| Jaeger | Tracing | 분산 트레이싱 |
| Helm | App Definition | K8s 패키지 매니저 |
| Argo | CI/CD | K8s GitOps/워크플로우 |
| Flux | CI/CD | K8s GitOps |
| Dapr | App Definition | 마이크로서비스 런타임 |
| TiKV | Database | 분산 KV 스토어 |
| Vitess | Database | MySQL 수평 확장 |
| CloudEvents | Streaming | 이벤트 메타데이터 표준 |
| Cilium | Networking | eBPF 기반 CNI + 네트워크 정책 |
| etcd | Orchestration | 분산 KV 스토어 (K8s 상태 저장) |
| OPA | Security | 범용 정책 엔진 |
| Istio | Service Mesh | 서비스 메시 컨트롤 플레인 |
| Linkerd | Service Mesh | 경량 서비스 메시 |
