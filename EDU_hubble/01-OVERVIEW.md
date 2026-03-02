# 01. 프로젝트 개요 (Overview)

## Hubble이란?

Hubble은 **Cilium 기반의 클라우드 네이티브 네트워크 옵저빌리티 플랫폼**입니다.
eBPF(extended Berkeley Packet Filter)를 활용하여 **애플리케이션 코드 변경 없이** 네트워크 트래픽의 심층 가시성을 제공합니다.

### 왜 Hubble이 필요한가?

전통적인 네트워크 모니터링 도구(tcpdump, Wireshark 등)는 쿠버네티스 환경에서 다음과 같은 한계를 가집니다:

1. **Pod 수명이 짧다**: IP가 동적으로 할당되므로 IP 기반 추적이 어렵다
2. **서비스 메시 복잡성**: East-West 트래픽이 많아 전체 흐름을 파악하기 힘들다
3. **L7 가시성 부족**: 패킷 캡처만으로는 DNS/HTTP/gRPC 레벨 인사이트를 얻기 어렵다
4. **분산 환경**: 멀티 노드 클러스터에서 네트워크 이벤트를 통합적으로 보기 어렵다

Hubble은 **커널 레벨(eBPF)**에서 데이터를 수집하기 때문에:
- sidecar proxy 없이 L3/L4/L7 가시성 제공
- Pod 이름, 네임스페이스, 레이블 등 쿠버네티스 메타데이터와 자동 연동
- 네트워크 정책(NetworkPolicy) 적용 결과를 실시간으로 관찰 가능

---

## 핵심 기능

| 기능 | 설명 | 안정성 |
|------|------|--------|
| **Flow Observation** | 실시간 네트워크 플로우 모니터링 (L3/L4/L7) | Stable |
| **Service Dependency Map** | 서비스 간 통신 관계를 자동으로 발견하고 시각화 | Stable |
| **Network Policy Observability** | 정책 허용/차단 판정 결과를 실시간으로 확인 | Stable |
| **DNS Monitoring** | DNS 쿼리/응답 모니터링, FQDN 기반 정책 연동 | Stable |
| **HTTP/gRPC Monitoring** | L7 프로토콜 레벨 요청/응답 관찰 | Stable |
| **Metrics & Alerting** | Prometheus 메트릭 내보내기 | Stable |
| **Multi-node Relay** | 멀티 노드 클러스터 이벤트 통합 | Stable |
| **Hubble UI** | 웹 기반 시각화 대시보드 | Beta |

---

## 기술 스택

### 언어 & 런타임

| 항목 | 값 | 선택 이유 |
|------|-----|-----------|
| **언어** | Go 1.24+ (toolchain 1.25.6) | Cilium 생태계 통일, 높은 동시성 처리 성능 |
| **최소 Go 버전** | 1.18 | 제네릭 지원을 위한 최소 요구사항 |

### 핵심 프레임워크 & 라이브러리

| 라이브러리 | 버전 | 역할 | 선택 이유 |
|-----------|------|------|-----------|
| `github.com/cilium/cilium` | v1.18.6 | 핵심 종속성 (Hubble 서버 코드 포함) | Hubble은 Cilium의 서브 프로젝트 |
| `google.golang.org/grpc` | v1.74.2 | 클라이언트-서버 통신 | 양방향 스트리밍, 프로토콜 버퍼 네이티브 지원 |
| `google.golang.org/protobuf` | v1.36.6 | 데이터 직렬화 | 스키마 기반, 다국어 지원, 높은 성능 |
| `github.com/spf13/cobra` | v1.9.1 | CLI 프레임워크 | Go 생태계 표준, 서브커맨드/플래그 관리 용이 |
| `github.com/spf13/viper` | v1.20.1 | 설정 관리 | 파일/환경변수/플래그 통합 설정 |
| `k8s.io/client-go` | v0.33.3 | 쿠버네티스 API 클라이언트 | 공식 클라이언트, port-forward 기능 |

### 인프라 기술

| 기술 | 역할 |
|------|------|
| **eBPF** | 커널 레벨 네트워크 이벤트 캡처 (Cilium 데이터플레인) |
| **gRPC** | Hubble CLI ↔ Server ↔ Relay 통신 프로토콜 |
| **Protocol Buffers** | Flow, Event 등 핵심 데이터 직렬화 |
| **Prometheus** | 메트릭 수집 및 내보내기 |
| **Kubernetes** | 실행 환경 (Pod, Service, Namespace 메타데이터 연동) |

---

## 프로젝트 구조

```
hubble/
├── main.go                  # CLI 진입점 → cmd.Execute() 호출
├── go.mod / go.sum          # Go 모듈 정의 및 의존성
├── Makefile                 # 빌드 자동화
├── stable.txt               # 안정 버전 정보
├── .github/                 # CI/CD (GitHub Actions)
│   └── workflows/
│       ├── ci.yml           # 단위 테스트, 린팅
│       └── release.yml      # 릴리스 자동화
├── Documentation/           # 공식 문서 (docs.cilium.io 링크)
├── install/kubernetes/      # 쿠버네티스 설치 매니페스트
├── policies/                # 네트워크 정책 예시
├── tutorials/               # 튜토리얼 리소스
└── vendor/                  # 벤더링된 종속성
    └── github.com/cilium/cilium/
        ├── hubble/
        │   ├── cmd/         # CLI 커맨드 구현
        │   └── pkg/         # CLI 유틸리티 (printer, logger 등)
        ├── pkg/hubble/      # Hubble 서버 핵심 코드
        │   ├── observer/    # 플로우 옵저버
        │   ├── parser/      # BPF 이벤트 파서
        │   ├── filters/     # 플로우 필터 시스템
        │   ├── metrics/     # 메트릭 수집
        │   ├── container/   # 링 버퍼
        │   └── peer/        # 피어 관리
        └── api/v1/          # Protobuf 정의
            ├── flow/        # 플로우 메시지 타입
            ├── observer/    # Observer gRPC 서비스
            ├── peer/        # Peer gRPC 서비스
            └── relay/       # Relay gRPC 서비스
```

### 중요: 저장소 구조에 대한 이해

이 저장소(`cilium/hubble`)는 **릴리스 아티팩트 및 배포 리소스**를 포함합니다.
실제 Hubble 핵심 코드는 `cilium/cilium` 저장소에서 관리되며, `vendor/` 디렉토리를 통해 포함됩니다.

- **코드 기여**: `cilium/cilium` 저장소의 `hubble/`, `pkg/hubble/` 디렉토리에 해야 함
- **CLI 이슈**: 이 저장소(`cilium/hubble`)에서 관리
- **서버/라이브러리 이슈**: `cilium/cilium` 저장소에서 관리

---

## 버전 정보

| 항목 | 값 |
|------|-----|
| **현재 버전** | v1.18.6 |
| **라이선스** | Apache-2.0 |
| **지원 플랫폼** | Linux, macOS, Windows (amd64, arm64) |
| **Docker 이미지** | `quay.io/cilium/hubble` |
| **호환성** | Hubble CLI는 지원되는 모든 Cilium 릴리스와 하위 호환 |

---

## 커뮤니티

- **Slack**: `#hubble` (사용자) / `#dev-hubble` (개발자)
- **GitHub**: [cilium/hubble](https://github.com/cilium/hubble)
- **공식 문서**: [docs.cilium.io](https://docs.cilium.io)
