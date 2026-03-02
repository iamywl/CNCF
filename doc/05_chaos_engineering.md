# Chaos Engineering

의도적으로 시스템에 장애를 주입하여 복원력(resilience)을 검증하는 도구들.

---

## 핵심 개념

카오스 엔지니어링은 Netflix가 2010년대 초 "Chaos Monkey"로 시작한 분야.

**원칙**:
1. 정상 상태(steady state)를 정의한다
2. 실험 그룹과 대조 그룹을 나눈다
3. 현실적인 장애를 주입한다 (Pod 킬, 네트워크 지연, CPU 과부하 등)
4. 정상 상태와의 차이를 측정한다

**목적**: "프로덕션에서 장애가 났을 때 시스템이 버티는가?"를 사전에 검증

---

## Incubating

### Chaos Mesh ★7.5k
- **역할**: K8s 카오스 엔지니어링 플랫폼 (PingCAP)
- **장애 유형**: Pod 킬, 네트워크 지연/손실/분할, I/O 장애, 시간 왜곡, DNS 장애, 커널 장애
- **특징**: Web UI 대시보드 제공, 워크플로우로 복합 실험 구성 가능
- **왜 쓰나**: K8s CRD로 카오스 실험을 선언적으로 정의하고 스케줄링
- **예시**:
  ```yaml
  apiVersion: chaos-mesh.org/v1alpha1
  kind: PodChaos
  metadata:
    name: pod-kill-example
  spec:
    action: pod-kill
    mode: one
    selector:
      labelSelectors:
        app: nginx
    scheduler:
      cron: "@every 10m"
  ```

### Litmus ★5.3k
- **역할**: K8s 카오스 실험 프레임워크 + ChaosHub
- **특징**:
  - **ChaosHub**: 사전 정의된 카오스 실험 라이브러리 (커뮤니티 공유)
  - **ChaosCenter**: 웹 기반 관리 콘솔
  - **Probes**: 실험 중 시스템 상태를 자동 검증하는 메커니즘
- **Chaos Mesh와의 차이**: Litmus는 실험 라이브러리(ChaosHub)가 강점, Chaos Mesh는 장애 유형의 깊이가 강점

---

## Sandbox

### Chaosblade ★6.3k
- **역할**: 카오스 주입 도구 (Alibaba)
- **특징**: K8s뿐 아니라 베어메탈, Docker, 클라우드 환경도 지원
- **장애 유형**: CPU/메모리 과부하, 네트워크 지연, 디스크 I/O, 프로세스 킬, JVM 장애 주입
- **비유**: 범용 장애 주입 도구 — 어떤 환경이든 카오스를 만들 수 있음

### Krkn ★440
- **역할**: K8s 카오스 테스트 도구 (Red Hat)
- **특징**: 병목 지점 식별, 복원력/성능 검증에 특화

---

## 주요 비-CNCF 도구

| 도구 | ★ | 설명 |
|---|---|---|
| **Chaos Toolkit** | 2k | 카오스 엔지니어링 오케스트레이션 프레임워크. 다양한 드라이버(K8s, AWS, GCP 등) |
| **chaoskube** | 1.9k | K8s Pod를 주기적으로 랜덤 킬. 가장 단순한 카오스 도구 |
| **KubeInvaders** | 1.1k | 게임화된 카오스 도구 (Space Invaders 스타일로 Pod 제거) |
| **PowerfulSeal** | 2k | K8s 클러스터 테스트 도구 (Bloomberg) |
| **Gremlin** | - | 상용 카오스 엔지니어링 SaaS (가장 유명한 상용 제품) |
| **steadybit** | - | 카오스를 넘어 종합 복원력 검증 플랫폼 |

---

## Chaos Mesh vs Litmus vs Chaosblade 비교

| | Chaos Mesh | Litmus | Chaosblade |
|---|---|---|---|
| **CNCF 등급** | Incubating | Incubating | Sandbox |
| **개발사** | PingCAP | Harness | Alibaba |
| **K8s 전용** | O | O | X (범용) |
| **Web UI** | O | O (ChaosCenter) | X (CLI) |
| **실험 라이브러리** | 내장 | ChaosHub (커뮤니티) | 내장 |
| **장애 깊이** | 깊음 (커널, 시간 왜곡) | 중간 | 넓음 (다양한 환경) |
| **워크플로우** | O | O | X |
| **적합한 상황** | K8s 심층 카오스 실험 | K8s + 실험 공유/관리 | 멀티 환경 카오스 |
