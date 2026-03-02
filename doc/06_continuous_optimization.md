# Continuous Optimization

클라우드 비용 최적화, 리소스 자동 튜닝, 성능 최적화를 다루는 카테고리.

K8s 운영 비용이 커지면서 최근 빠르게 성장하는 분야. FinOps(Financial Operations)와 밀접하게 연관.

---

## 핵심 개념

- **FinOps**: 클라우드 비용을 엔지니어링 관점에서 관리하는 문화/실천. "비용도 성능 지표의 하나"
- **Right-sizing**: 워크로드에 맞게 리소스(CPU, 메모리)를 적정 수준으로 조정
- **Spot/Preemptible Instance**: 유휴 클라우드 자원을 저가에 활용 (중단 가능)
- **Bin Packing**: Pod를 최소 노드에 밀집 배치하여 유휴 노드를 줄임

---

## CNCF 공식 프로젝트

이 카테고리에는 Graduated/Incubating/Sandbox 프로젝트가 없다. 대부분 상용 SaaS 또는 독립 오픈소스.

---

## 비용 최적화 도구

| 도구 | 유형 | 설명 |
|---|---|---|
| **CAST AI** | SaaS | K8s 클라우드 비용 50%+ 절감 자동화. 노드 자동 교체, Right-sizing, Spot 활용. AWS/Azure/GCP 지원 |
| **CloudPilot AI** | SaaS | K8s 비용 80% 절감. 5분 배포, 성능 영향 제로 주장 |
| **CloudZero** | SaaS | 클라우드 지출 분석/관리 플랫폼. 비용을 팀/기능/고객별로 할당 |
| **gocrane (Crane)** | 오픈소스 | K8s FinOps 플랫폼. 비용 가시화 + 리소스 추천 + 자동 스케일링. CNCF 외부 프로젝트 |

## 성능/리소스 최적화 도구

| 도구 | 유형 | 설명 |
|---|---|---|
| **Akamas** | SaaS | AI 기반 워크로드 성능/비용/복원력 자동 최적화. JVM, DB, K8s 파라미터 튜닝 |
| **BMC Helix** | 엔터프라이즈 | 클라우드/IT 관리 플랫폼. AIOps 기반 운영 자동화 |

## 관측성/분석 도구

| 도구 | 유형 | 설명 |
|---|---|---|
| **ControlTheory** | SaaS/OSS | SRE용 텔레메트리 분석 도구. 관측 데이터를 실행 가능한 인사이트로 변환 |

---

## FinOps 실천 가이드

### 1단계: Inform (가시화)
- 현재 비용을 팀/서비스/환경별로 분류
- 도구: CloudZero, Crane

### 2단계: Optimize (최적화)
- Right-sizing: 과잉 할당된 CPU/메모리 줄이기
- Spot Instance 활용
- 유휴 리소스 정리 (미사용 PV, 중지된 노드)
- 도구: CAST AI, CloudPilot AI, Akamas

### 3단계: Operate (운영)
- 비용 예산/알림 설정
- 자동 스케일링 정책 튜닝
- 정기적 비용 리뷰 문화
- 도구: Crane (자동 추천), CAST AI (자동 적용)

---

## K8s 비용 최적화 핵심 전략

| 전략 | 설명 | 절감률 |
|---|---|---|
| **Right-sizing** | Request/Limit을 실제 사용량에 맞춤 | 20-40% |
| **Spot Instance** | 중단 허용 워크로드에 Spot 노드 사용 | 60-90% |
| **Cluster Autoscaler** | 필요할 때만 노드 추가, 유휴 시 제거 | 20-30% |
| **HPA(Horizontal Pod Autoscaler)** | 트래픽에 따라 Pod 수 자동 조정 | 가변 |
| **Namespace 쿼터** | 팀별 리소스 상한 설정으로 과잉 방지 | 가변 |
