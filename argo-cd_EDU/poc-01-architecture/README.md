# PoC 01: Argo CD 멀티 컴포넌트 아키텍처

## 개요

Argo CD는 **단일 바이너리**에 여러 컴포넌트가 통합된 아키텍처를 채택한다. 실행 시 `argv[0]` 또는 `ARGOCD_BINARY_NAME` 환경변수로 역할을 결정하며, `cmux` 라이브러리로 단일 포트에서 gRPC/HTTP를 동시에 처리한다. 이 PoC는 그 핵심 메커니즘을 Go 표준 라이브러리만으로 시뮬레이션한다.

## 다루는 개념

| 개념 | 설명 | 실제 소스 |
|------|------|-----------|
| 단일 바이너리 디스패처 | `os.Args[0]` 기반 컴포넌트 선택 | `cmd/main.go` |
| cmux 포트 멀티플렉싱 | 8080 포트에서 gRPC/HTTP/gRPC-Web 동시 수신 | `server/server.go` |
| 컴포넌트 간 gRPC 통신 | API Server ↔ Repo Server ↔ Controller | `server/application/application.go` |
| K8s Secret 기반 저장소 | 클러스터/레포지토리 정보를 Secret으로 관리 | `util/db/cluster.go`, `util/db/repository.go` |
| 전체 요청 흐름 | Client → API → Repo → Controller → Cluster | `controller/appcontroller.go` |

## 아키텍처 다이어그램

```
단일 바이너리 (argocd)
│
├─ argocd-server (:8080)
│    └─ cmux
│         ├─ gRPC (content-type: application/grpc)
│         ├─ gRPC-Web (브라우저)
│         └─ HTTP/1.1 (REST API, Web UI)
│
├─ argocd-repo-server (:8081)
│    └─ gRPC only (매니페스트 생성)
│
├─ argocd-application-controller (:8082)
│    ├─ appRefreshQueue (상태 갱신)
│    └─ appOperationQueue (Sync 실행)
│
└─ argocd-dex-server (:5556)
     └─ OIDC Provider

K8s Secret 저장소 (argocd 네임스페이스)
├─ label: secret-type=cluster    → 대상 클러스터 연결 정보
└─ label: secret-type=repository → Git/Helm 인증 정보
```

## 실행 방법

```bash
cd poc-01-architecture
go run main.go
```

### 예상 출력

```
=================================================================
 Argo CD 멀티 컴포넌트 아키텍처 시뮬레이션
=================================================================

[ 1단계: 단일 바이너리 디스패처 ]
  argocd-dex-server    → OIDC Provider
  argocd-server        → API Server + UI
  argocd-repo-server   → Repository Server
  argocd-application-controller → GitOps Controller

  현재 엔트리포인트: argocd-server (API Server + UI)

[ 2단계: 컴포넌트 시작 ]
[Repo Server] 시작 — 포트 :8081 (gRPC only)
[Controller] 시작 — Application Controller
[API Server] 시작 — 포트 :8080 (gRPC + HTTP/gRPC-Web 멀티플렉싱)
...
```

## 참조 소스코드

| 파일 | 설명 |
|------|------|
| `cmd/main.go` | 단일 바이너리 진입점, 컴포넌트 디스패치 |
| `server/server.go` | `ArgoCDServer` 구조체, cmux 초기화 |
| `reposerver/server.go` | `Server` 구조체, 매니페스트 생성 |
| `controller/appcontroller.go` | `ApplicationController`, 워크큐 |
| `util/db/cluster.go` | 클러스터 Secret 읽기/쓰기 |
| `util/db/repository.go` | 레포지토리 Secret 읽기/쓰기 |

## 핵심 설계 결정

**왜 단일 바이너리인가?**
배포 단순화를 위해 하나의 컨테이너 이미지로 모든 컴포넌트를 패키징한다. Kubernetes의 명령어(command) 오버라이드만으로 각 컴포넌트를 독립적으로 실행할 수 있다.

**왜 cmux인가?**
TLS 인증서를 하나의 포트에 집중시켜 인증서 관리를 단순화하면서, gRPC(HTTP/2)와 REST(HTTP/1.1)를 동시에 지원해야 하는 요구사항을 충족한다.

**왜 K8s Secret인가?**
별도의 데이터베이스 없이 Kubernetes etcd를 단일 저장소로 활용하여, `kubectl`로 모든 상태를 직접 조회/수정할 수 있다. 클러스터 연결 정보에는 자격증명이 포함되므로 Secret의 RBAC 보호를 자연스럽게 활용한다.
