# CNCF & Cloud Native Open Source Study

CNCF(Cloud Native Computing Foundation) 및 클라우드 네이티브 생태계 오픈소스 프로젝트들의 **소스코드 분석 + 교육 자료(EDU)** 모노레포.

16개 프로젝트의 소스코드를 직접 분석하고, 각 프로젝트별 기본 문서 7개 + 심화 문서 13개 + PoC 코드를 포함한 체계적인 교육 자료를 제공합니다.

---

## 프로젝트 목록

### Container & Runtime

<table>
  <tr>
    <td align="center" width="140">
      <a href="https://kubernetes.io/">
        <img src="https://raw.githubusercontent.com/cncf/artwork/main/projects/kubernetes/icon/color/kubernetes-icon-color.svg" width="60" alt="Kubernetes"/>
      </a>
      <br/><b>Kubernetes</b>
      <br/><sub>컨테이너 오케스트레이션</sub>
      <br/><a href="kubernetes_EDU/">EDU</a> · <a href="kubernetes/">Source</a>
    </td>
    <td align="center" width="140">
      <a href="https://containerd.io/">
        <img src="https://raw.githubusercontent.com/cncf/artwork/main/projects/containerd/icon/color/containerd-icon-color.png" width="60" alt="containerd"/>
      </a>
      <br/><b>containerd</b>
      <br/><sub>컨테이너 런타임</sub>
      <br/><a href="containerd_EDU/">EDU</a> · <a href="containerd/">Source</a>
    </td>
    <td align="center" width="140">
      <a href="https://github.com/cirruslabs/tart">
        <img src="https://github.com/cirruslabs/tart/raw/main/Resources/TartSocial.png" width="60" alt="Tart"/>
      </a>
      <br/><b>Tart</b>
      <br/><sub>macOS VM 가상화</sub>
      <br/><a href="tart_EDU/">EDU</a> · <a href="tart/">Source</a>
    </td>
  </tr>
</table>

### Networking & Service Mesh

<table>
  <tr>
    <td align="center" width="140">
      <a href="https://cilium.io/">
        <img src="https://raw.githubusercontent.com/cncf/artwork/main/projects/cilium/icon/color/cilium_icon-color.png" width="60" alt="Cilium"/>
      </a>
      <br/><b>Cilium</b>
      <br/><sub>eBPF 기반 네트워킹</sub>
      <br/><a href="cilium_EDU/">EDU</a> · <a href="cilium/">Source</a>
    </td>
    <td align="center" width="140">
      <a href="https://istio.io/">
        <img src="https://raw.githubusercontent.com/cncf/artwork/main/projects/istio/icon/color/istio-icon-color.png" width="60" alt="Istio"/>
      </a>
      <br/><b>Istio</b>
      <br/><sub>서비스 메시</sub>
      <br/><a href="istio_EDU/">EDU</a> · <a href="istio/">Source</a>
    </td>
    <td align="center" width="140">
      <a href="https://grpc.io/">
        <img src="https://raw.githubusercontent.com/cncf/artwork/main/projects/grpc/icon/color/grpc-icon-color.png" width="60" alt="gRPC"/>
      </a>
      <br/><b>gRPC-Go</b>
      <br/><sub>고성능 RPC 프레임워크</sub>
      <br/><a href="grpc-go_EDU/">EDU</a> · <a href="grpc-go/">Source</a>
    </td>
    <td align="center" width="140">
      <a href="https://github.com/cilium/hubble">
        <img src="https://raw.githubusercontent.com/cilium/hubble/main/Documentation/images/hubble_logo.png" width="60" alt="Hubble"/>
      </a>
      <br/><b>Hubble</b>
      <br/><sub>네트워크 옵저버빌리티</sub>
      <br/><a href="hubble_EDU/">EDU</a>
    </td>
  </tr>
</table>

### Observability & Monitoring

<table>
  <tr>
    <td align="center" width="140">
      <a href="https://grafana.com/">
        <img src="https://raw.githubusercontent.com/grafana/grafana/main/public/img/grafana_icon.svg" width="60" alt="Grafana"/>
      </a>
      <br/><b>Grafana</b>
      <br/><sub>메트릭 시각화 플랫폼</sub>
      <br/><a href="grafana_EDU/">EDU</a> · <a href="grafana/">Source</a>
    </td>
    <td align="center" width="140">
      <a href="https://prometheus.io/docs/alerting/latest/alertmanager/">
        <img src="https://raw.githubusercontent.com/cncf/artwork/main/projects/prometheus/icon/color/prometheus-icon-color.png" width="60" alt="Alertmanager"/>
      </a>
      <br/><b>Alertmanager</b>
      <br/><sub>알림 관리 시스템</sub>
      <br/><a href="alertmanager_EDU/">EDU</a> · <a href="alertmanager/">Source</a>
    </td>
    <td align="center" width="140">
      <a href="https://grafana.com/oss/loki/">
        <img src="https://raw.githubusercontent.com/grafana/loki/main/docs/sources/logo.png" width="60" alt="Loki"/>
      </a>
      <br/><b>Loki</b>
      <br/><sub>로그 수집 시스템</sub>
      <br/><a href="loki_EDU/">EDU</a> · <a href="loki/">Source</a>
    </td>
    <td align="center" width="140">
      <a href="https://www.jaegertracing.io/">
        <img src="https://raw.githubusercontent.com/cncf/artwork/main/projects/jaeger/icon/color/jaeger-icon-color.png" width="60" alt="Jaeger"/>
      </a>
      <br/><b>Jaeger</b>
      <br/><sub>분산 트레이싱</sub>
      <br/><a href="jaeger_EDU/">EDU</a> · <a href="jaeger/">Source</a>
    </td>
  </tr>
</table>

### CI/CD & Infrastructure

<table>
  <tr>
    <td align="center" width="140">
      <a href="https://argoproj.github.io/cd/">
        <img src="https://raw.githubusercontent.com/cncf/artwork/main/projects/argo/icon/color/argo-icon-color.png" width="60" alt="Argo CD"/>
      </a>
      <br/><b>Argo CD</b>
      <br/><sub>GitOps 지속적 배포</sub>
      <br/><a href="argo-cd_EDU/">EDU</a> · <a href="argo-cd/">Source</a>
    </td>
    <td align="center" width="140">
      <a href="https://www.jenkins.io/">
        <img src="https://www.jenkins.io/images/logos/jenkins/jenkins.svg" width="60" alt="Jenkins"/>
      </a>
      <br/><b>Jenkins</b>
      <br/><sub>CI/CD 자동화 서버</sub>
      <br/><a href="jenkins_EDU/">EDU</a> · <a href="jenkins/">Source</a>
    </td>
    <td align="center" width="140">
      <a href="https://helm.sh/">
        <img src="https://raw.githubusercontent.com/cncf/artwork/main/projects/helm/icon/color/helm-icon-color.png" width="60" alt="Helm"/>
      </a>
      <br/><b>Helm</b>
      <br/><sub>Kubernetes 패키지 매니저</sub>
      <br/><a href="helm_EDU/">EDU</a> · <a href="helm/">Source</a>
    </td>
    <td align="center" width="140">
      <a href="https://www.terraform.io/">
        <img src="https://www.datocms-assets.com/2885/1620155116-brandhcterraformverticalcolor.svg" width="60" alt="Terraform"/>
      </a>
      <br/><b>Terraform</b>
      <br/><sub>인프라스트럭처 as Code</sub>
      <br/><a href="terraform_EDU/">EDU</a> · <a href="terraform/">Source</a>
    </td>
  </tr>
</table>

### Streaming & Messaging

<table>
  <tr>
    <td align="center" width="140">
      <a href="https://kafka.apache.org/">
        <img src="https://raw.githubusercontent.com/apache/kafka/trunk/docs/images/kafka-logo-readme-light.svg" width="60" alt="Kafka"/>
      </a>
      <br/><b>Apache Kafka</b>
      <br/><sub>이벤트 스트리밍 플랫폼</sub>
      <br/><a href="kafka_EDU/">EDU</a> · <a href="kafka/">Source</a>
    </td>
  </tr>
</table>

---

## EDU 자료 구성

각 프로젝트의 EDU는 다음과 같은 구조로 구성됩니다:

| 구분 | 문서 | 내용 |
|------|------|------|
| **기본** | `README.md` | 프로젝트 개요 및 EDU 목차 |
| **기본** | `01-architecture.md` | 전체 아키텍처, 컴포넌트 관계 |
| **기본** | `02-data-model.md` | 핵심 데이터 구조, 스키마 |
| **기본** | `03-sequence-diagrams.md` | 주요 요청 흐름 시퀀스 다이어그램 |
| **기본** | `04-code-structure.md` | 디렉토리 구조, 빌드 시스템 |
| **기본** | `05-core-components.md` | 핵심 컴포넌트 동작 원리 |
| **기본** | `06-operations.md` | 배포, 설정, 모니터링 |
| **심화** | `07~18-*.md` | 서브시스템별 deep-dive (각 500줄+) |
| **실습** | `poc-*/main.go` | 핵심 개념 PoC (표준 라이브러리만 사용) |

### 작성 원칙

- 소스코드를 **직접 분석**한 내용만 포함 (추측 금지)
- 코드 참조는 `Grep`/`Read`로 **검증된 경로/함수명**만 인용
- PoC는 `go run main.go`로 **실행 확인** 완료
- 전체 **한국어**로 작성

---

## 전체 현황

| 프로젝트 | 기본 문서 | 심화 문서 | PoC | 상태 |
|----------|:---------:|:---------:|:---:|:----:|
| Cilium | 7 | 13 | 28 | **완료** |
| Kubernetes | 7 | 13 | 35 | **완료** |
| Argo CD | 7 | 13 | 19 | **완료** |
| Jenkins | 7 | 13 | 26 | **완료** |
| containerd | 7 | 13 | 24 | **완료** |
| gRPC-Go | 7 | 13 | 25 | **완료** |
| Tart | 7 | 13 | 17 | **완료** |
| Helm | 7 | 13 | 19 | **완료** |
| Terraform | 7 | 13 | 22 | **완료** |
| Alertmanager | 7 | 13 | 21 | **완료** |
| Hubble | 7 | 13 | 17 | **완료** |
| Kafka | 7 | 13 | 23 | **완료** |
| Grafana | 7 | 13 | 30 | **완료** |
| Loki | 7 | 13 | 21 | **완료** |
| Jaeger | 7 | 13 | 22 | **완료** |
| Istio | 7 | 13 | 21 | **완료** |
| **합계** | **112** | **208** | **370** | **16/16 완료** |

---

## 참고 링크

- [CNCF Landscape](https://landscape.cncf.io/)
- [CNCF Projects](https://www.cncf.io/projects/)
- [CNCF TOC](https://github.com/cncf/toc)
- [CNCF Artwork](https://github.com/cncf/artwork) (프로젝트 로고 출처)
