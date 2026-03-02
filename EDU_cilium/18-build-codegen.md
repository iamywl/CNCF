# 18. Cilium 빌드 시스템 및 코드 생성 (Build System & Code Generation)

## 목차
1. [개요](#1-개요)
2. [Makefile 구조](#2-makefile-구조)
3. [Go 빌드 시스템](#3-go-빌드-시스템)
4. [BPF 프로그램 컴파일](#4-bpf-프로그램-컴파일)
5. [Docker 멀티스테이지 빌드](#5-docker-멀티스테이지-빌드)
6. [Helm 3 차트](#6-helm-3-차트)
7. [코드 생성 - Protobuf](#7-코드-생성---protobuf)
8. [코드 생성 - go-swagger](#8-코드-생성---go-swagger)
9. [코드 생성 - deepcopy-gen](#9-코드-생성---deepcopy-gen)
10. [코드 생성 - controller-gen (CRD)](#10-코드-생성---controller-gen-crd)
11. [코드 생성 - client-gen](#11-코드-생성---client-gen)
12. [CI/CD 파이프라인](#12-cicd-파이프라인)
13. [테스트 프레임워크](#13-테스트-프레임워크)
14. [개발 도구 및 워크플로우](#14-개발-도구-및-워크플로우)
15. [PoC 안내](#15-poc-안내)

---

## 1. 개요

Cilium 프로젝트는 대규모 클라우드 네이티브 네트워킹 프로젝트로서, Go 바이너리, BPF C 프로그램, Kubernetes CRD, Helm 차트, Docker 이미지 등 다양한 아티팩트를 생산하는 복합 빌드 시스템을 가지고 있다. 이 문서에서는 Cilium의 빌드 시스템 아키텍처와 자동 코드 생성 파이프라인을 심층 분석한다.

### 빌드 시스템 전체 아키텍처

```
소스 코드                    빌드 도구                      생산물
==========                 ==========                   ==========

*.proto files  ---protoc--> *.pb.go (gRPC 코드)
openapi.yaml   ---swagger-> server/, client/, models/ (REST API 코드)
Go types       ---deepcopy-gen--> zz_generated.deepcopy.go
Go types       ---controller-gen-> CRD YAML 매니페스트
Go types       ---client-gen----> typed Kubernetes 클라이언트
*.c (BPF)      ---clang--------> *.o (ELF BPF 바이트코드)
Go packages    ---go build-----> cilium-agent, operator 등 바이너리
Dockerfile     ---docker build-> 컨테이너 이미지
Helm templates ---helm-------->  Kubernetes 배포 매니페스트
```

### 핵심 파일 경로

| 파일/디렉토리 | 역할 |
|---|---|
| `Makefile` | 루트 빌드 진입점 |
| `Makefile.defs` | 공통 변수/플래그 정의 |
| `Makefile.quiet` | 빌드 출력 제어 (V=0 조용, V=1 상세) |
| `Makefile.docker` | Docker 이미지 빌드 템플릿 |
| `Makefile.kind` | Kind 클러스터 개발 환경 |
| `api/v1/Makefile.protoc` | Protobuf 컴파일 규칙 |
| `bpf/Makefile` | BPF 프로그램 빌드 |
| `bpf/Makefile.bpf` | BPF 공통 컴파일 플래그 |
| `contrib/scripts/k8s-code-gen.sh` | Kubernetes 코드 생성 |
| `contrib/scripts/k8s-manifests-gen.sh` | CRD 매니페스트 생성 |

---

## 2. Makefile 구조

Cilium의 Makefile 시스템은 계층적 구조를 가진다. 루트 Makefile이 전체 빌드를 오케스트레이션하고, 각 서브디렉토리의 Makefile이 개별 컴포넌트를 빌드한다.

### 2.1 루트 Makefile (`Makefile`)

루트 Makefile은 프로젝트의 빌드 진입점이다.

**파일 경로**: `Makefile`

```makefile
# 기본 타겟: precheck -> build -> postcheck
all: precheck build postcheck
    @echo "Build finished."

# 서브디렉토리 정의
SUBDIRS_CILIUM_CONTAINER := cilium-dbg daemon cilium-health bugtool hubble \
    tools/mount tools/sysctlfix plugins/cilium-cni
SUBDIR_OPERATOR_CONTAINER := operator
SUBDIR_RELAY_CONTAINER := hubble-relay
SUBDIR_CLUSTERMESH_APISERVER_CONTAINER := clustermesh-apiserver

# 전체 빌드 서브디렉토리
SUBDIRS := $(SUBDIRS_CILIUM_CONTAINER) $(SUBDIR_OPERATOR_CONTAINER) plugins \
    tools $(SUBDIR_RELAY_CONTAINER) bpf $(SUBDIR_CLUSTERMESH_APISERVER_CONTAINER)

# 각 서브디렉토리에 대해 재귀적으로 make 호출
build: $(SUBDIRS)

$(SUBDIRS): force
    @ $(MAKE) $(SUBMAKEOPTS) -C $@ all
```

**핵심 빌드 타겟들**:

| 타겟 | 설명 |
|---|---|
| `all` | 전체 빌드 (precheck -> build -> postcheck) |
| `build` | 모든 서브디렉토리 빌드 |
| `build-container` | cilium-agent 컨테이너용 컴포넌트만 빌드 |
| `build-container-operator` | cilium-operator 컨테이너용 빌드 |
| `debug` | NOOPT=1, NOSTRIP=1로 디버그 빌드 |
| `clean` | 빌드 산출물 제거 |
| `install` | 빌드된 바이너리 설치 |

### 2.2 Makefile.defs - 공통 정의

**파일 경로**: `Makefile.defs`

이 파일은 프로젝트 전반에서 사용되는 변수, 플래그, 도구 경로를 정의한다.

```makefile
# 셸 설정 - 에러 발생 시 즉시 중단, 파이프라인 실패 전파
SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

# Go 빌드 환경
export GO ?= go
NATIVE_ARCH = $(shell GOARCH= $(GO) env GOARCH)
export GOARCH ?= $(NATIVE_ARCH)
CGO_ENABLED ?= 0

# Go 빌드 플래그 조합
GO_BUILD_FLAGS += -mod=vendor
GO_BUILD_FLAGS += -ldflags '$(GO_BUILD_LDFLAGS)' -tags=$(call join-with-comma,$(GO_TAGS_FLAGS))

# 최종 빌드 명령어
GO_BUILD = $(GO_BUILD_ENV) $(GO) build $(GO_BUILD_FLAGS)
GO_TEST = CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS)

# 버전 정보를 바이너리에 주입
VERSION = $(shell cat $(dir $(lastword $(MAKEFILE_LIST)))/VERSION)
GIT_VERSION = $(shell git show -s --format='format:%h %aI')
GO_BUILD_LDFLAGS += -X "github.com/cilium/cilium/pkg/version.ciliumVersion=$(FULL_BUILD_VERSION)"

# 바이너리 최적화 플래그
ifeq ($(NOSTRIP),)
    GO_BUILD_LDFLAGS += -s -w  # DWARF 심볼 테이블과 디버그 정보 제거
endif

# 크로스 컴파일 지원
ifeq ($(CROSS_ARCH),arm64)
    GO_BUILD_ENV += CC=aarch64-linux-gnu-gcc
else ifeq ($(CROSS_ARCH),amd64)
    GO_BUILD_ENV += CC=x86_64-linux-gnu-gcc
endif

# Swagger (OpenAPI) 코드 생성 도구
SWAGGER_VERSION = 0.33.1
SWAGGER := $(CONTAINER_ENGINE) run ... quay.io/goswagger/swagger:$(SWAGGER_VERSION)
```

**핵심 변수들**:

| 변수 | 기본값 | 설명 |
|---|---|---|
| `CGO_ENABLED` | `0` | CGO 비활성화 (순수 Go 빌드) |
| `GO_BUILD_FLAGS` | `-mod=vendor` | 벤더 디렉토리 사용 |
| `NOSTRIP` | (비어있음) | 설정 시 디버그 심볼 유지 |
| `NOOPT` | (비어있음) | 설정 시 컴파일러 최적화 비활성화 |
| `RACE` | (비어있음) | 설정 시 race detector 활성화 |
| `LOCKDEBUG` | (비어있음) | 설정 시 lock 디버깅 활성화 |

### 2.3 Makefile.quiet - 출력 제어

**파일 경로**: `Makefile.quiet`

```makefile
ifeq ($(V),0)
    QUIET=@              # 명령어 에코 억제
    ECHO_CC=echo "  CC     $(RELATIVE_DIR)/$@"
    ECHO_CHECK=echo "  CHECK  $(RELATIVE_DIR)"
    ECHO_GEN=echo "  GEN    $(RELATIVE_DIR)/"
    ECHO_GO=echo "  GO     $(RELATIVE_DIR)/$@"
    SUBMAKEOPTS="-s"     # 서브 make도 조용하게
else
    # 빈 값 = 기본 make 동작 (상세 출력)
    ECHO_CC=:
    ECHO_GO=:
    SUBMAKEOPTS=
endif
```

`V=0` (기본값)일 때 빌드 출력이 "CC", "GO", "GEN" 등의 축약형 프리픽스로 표시되어 가독성이 좋다. `V=1`로 설정하면 실제 실행 명령어가 모두 출력된다.

### 2.4 Makefile 의존성 그래프

```
Makefile (루트)
  |-- include Makefile.defs
  |     |-- include Makefile.quiet
  |-- include Makefile.kind
  |-- include Makefile.docker
  |-- -include Makefile.override (선택적)
  |
  |-- daemon/Makefile (cilium-agent)
  |-- operator/Makefile (cilium-operator)
  |-- hubble-relay/Makefile (hubble-relay)
  |-- bpf/Makefile
  |     |-- include ../Makefile.defs
  |     |-- include Makefile.bpf
  |-- plugins/cilium-cni/Makefile
  |-- ...
```

---

## 3. Go 빌드 시스템

### 3.1 Go 빌드 명령어 구성

Cilium의 Go 빌드는 `Makefile.defs`에서 정의된 `GO_BUILD` 매크로를 통해 실행된다.

```makefile
# 최종 빌드 명령어 구성
GO_BUILD = $(GO_BUILD_ENV) $(GO) build $(GO_BUILD_FLAGS)

# 예시: 실제 실행되는 명령어
# CGO_ENABLED=0 GOARCH=amd64 go build \
#   -mod=vendor \
#   -ldflags '-s -w -X "github.com/cilium/cilium/pkg/version.ciliumVersion=1.20.0-dev abc1234 2025-01-15"' \
#   -tags=osusergo \
#   -o cilium-agent
```

### 3.2 서브디렉토리 빌드 패턴

각 컴포넌트는 독립적인 Makefile을 가진다.

**파일 경로**: `daemon/Makefile`

```makefile
include ${ROOT_DIR}/../Makefile.defs

TARGET := cilium-agent

$(TARGET):
    @$(ECHO_GO)
    $(QUIET)$(GO_BUILD) -o $(TARGET)
```

### 3.3 ldflags를 통한 버전 정보 주입

빌드 시 `-ldflags`를 통해 바이너리에 메타데이터가 주입된다:

```makefile
# VERSION 파일에서 버전 읽기
VERSION = $(shell cat VERSION)
# Git에서 커밋 해시와 날짜 추출
GIT_VERSION = $(shell git show -s --format='format:%h %aI')

# 바이너리에 주입할 값
GO_BUILD_LDFLAGS += -X "github.com/cilium/cilium/pkg/version.ciliumVersion=$(VERSION) $(GIT_VERSION)"
# Envoy 이미지 SHA도 주입
GO_BUILD_LDFLAGS += -X "github.com/cilium/cilium/pkg/envoy.requiredEnvoyVersionSHA=$(CILIUM_ENVOY_SHA)"
```

### 3.4 빌드 태그

```makefile
GO_TAGS_FLAGS += osusergo        # 순수 Go os/user 패키지 사용
ifneq ($(LOCKDEBUG),)
    GO_TAGS_FLAGS += lockdebug   # lock 디버깅
endif
ifneq ($(RACE),)
    GO_BUILD_FLAGS += -race      # race detector (CGO 필요)
    CGO_ENABLED = 1
endif
```

### 3.5 크로스 컴파일

```makefile
# arm64 크로스 컴파일
make GOARCH=arm64

# 내부적으로 다음이 설정됨:
# GO_BUILD_ENV += CC=aarch64-linux-gnu-gcc
# CGO가 필요한 경우 적절한 크로스 컴파일러 사용
```

---

## 4. BPF 프로그램 컴파일

Cilium의 핵심인 BPF(eBPF) 프로그램은 C로 작성되며 clang으로 컴파일된다.

### 4.1 BPF 컴파일 도구 체인

**파일 경로**: `bpf/Makefile.bpf`

```makefile
# 컴파일 플래그
FLAGS := -I$(ROOT_DIR)/bpf -I$(ROOT_DIR)/bpf/include -O2 -g
CLANG_FLAGS := ${FLAGS} --target=bpf -std=gnu99 -nostdinc
CLANG_FLAGS += -Wall -Wextra -Werror -Wshadow
CLANG_FLAGS += -mcpu=v3       # BPF ISA v3 (커널 5.7+)

# 컴파일 규칙: .c -> .o (BPF ELF 오브젝트)
%.o: %.c $(LIB)
    ${CLANG} ${CLANG_FLAGS} -c $< -o $@
```

### 4.2 BPF 프로그램 목록

**파일 경로**: `bpf/Makefile`

```makefile
# 기본 BPF 프로그램
BPF_SIMPLE = bpf_network.o bpf_alignchecker.o

# 전체 BPF 프로그램 (컴파일 옵션 순열 테스트 포함)
BPF = bpf_lxc.o bpf_overlay.o bpf_sock.o bpf_host.o \
      bpf_wireguard.o bpf_xdp.o $(BPF_SIMPLE)
```

| BPF 프로그램 | 기능 |
|---|---|
| `bpf_lxc.o` | 컨테이너/엔드포인트 네트워킹 (가장 복잡) |
| `bpf_host.o` | 호스트 네트워킹, 방화벽 |
| `bpf_overlay.o` | 오버레이 네트워킹 (VXLAN/Geneve) |
| `bpf_xdp.o` | XDP 고속 패킷 처리 |
| `bpf_sock.o` | 소켓 레벨 로드밸런싱 |
| `bpf_wireguard.o` | WireGuard 암호화 |
| `bpf_network.o` | 네트워크 기본 기능 |

### 4.3 BPF 빌드 옵션 순열 (Permutation Build)

Cilium은 다양한 기능 조합의 BPF 프로그램을 컴파일 테스트한다:

```makefile
# 로드밸런서 옵션 순열
LB_OPTIONS = \
    -DSKIP_DEBUG: \
    -DENABLE_IPV4: \
    -DENABLE_IPV4:-DENCAP_IFINDEX:-DTUNNEL_MODE: \
    -DENABLE_IPV4:-DENABLE_IPV6:-DENCAP_IFINDEX:-DTUNNEL_MODE:-DENABLE_NODEPORT: \
    ...

# 최대 복잡도 옵션 (BPF 프로그램 크기/복잡도 한계 테스트)
MAX_BASE_OPTIONS = -DSKIP_DEBUG=1 -DENABLE_IPV4=1 -DENABLE_IPV6=1 \
    -DENABLE_ROUTING=1 -DPOLICY_VERDICT_NOTIFY=1 ...
```

이러한 순열 테스트는 특정 기능 조합에서만 발생하는 BPF 컴파일 에러나 검증기(verifier) 실패를 미리 감지한다.

### 4.4 BPF 코드 생성 (`make generate-bpf`)

```makefile
generate-bpf:
    # dpgen: BPF 오브젝트에서 config 구조체 생성
    $(GO) generate ../pkg/datapath/config
    # bpf2go: BPF 오브젝트에서 Go 스켈레톤 코드 생성
    BPF2GO_CC="$(CLANG)" BPF2GO_CFLAGS="..." $(GO) generate ../pkg/datapath/bpf
```

---

## 5. Docker 멀티스테이지 빌드

### 5.1 Dockerfile 구조

**파일 경로**: `images/cilium/Dockerfile`

Cilium의 Docker 빌드는 멀티스테이지 패턴을 사용한다:

```dockerfile
# Stage 1: Envoy 프록시 이미지에서 바이너리 추출
ARG CILIUM_ENVOY_IMAGE=quay.io/cilium/cilium-envoy:v1.36.5-...
FROM ${CILIUM_ENVOY_IMAGE} AS cilium-envoy

# Stage 2: 빌더 이미지에서 Go 바이너리 컴파일
FROM --platform=${BUILDPLATFORM} ${CILIUM_BUILDER_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium \
    --mount=type=cache,target=/root/.cache \
    --mount=type=cache,target=/go/pkg \
    make GOARCH=${TARGETARCH} DESTDIR=/tmp/install/${TARGETOS}/${TARGETARCH} \
    build-container install-container-binary

# Stage 3: 릴리즈 이미지 (런타임만 포함)
FROM ${CILIUM_RUNTIME_IMAGE} AS release
COPY --from=cilium-envoy /usr/lib/libcilium.so /usr/lib/libcilium.so
COPY --from=cilium-envoy /usr/bin/cilium-envoy /usr/bin/
COPY --from=builder /tmp/install/${TARGETOS}/${TARGETARCH} /

# Stage 4: 디버그 이미지 (릴리즈 + delve 디버거)
FROM release AS debug
COPY --from=builder /tmp/install/.../cilium-agent /usr/bin/cilium-agent-bin
COPY --from=debug-tools /go/bin/dlv /usr/bin/dlv
```

### 5.2 Docker 빌드 특징

1. **BuildKit 캐시 마운트**: `--mount=type=cache`로 Go 모듈 캐시와 빌드 캐시 재사용
2. **크로스 플랫폼**: `BUILDPLATFORM`과 `TARGETARCH`로 멀티 아키텍처 빌드
3. **디버그 심볼 분리**: `objcopy --only-keep-debug`로 별도 .debug 파일 생성

### 5.3 Makefile.docker - 이미지 빌드 템플릿

**파일 경로**: `Makefile.docker`

```makefile
# Docker 이미지 빌드 템플릿 매크로
define DOCKER_IMAGE_TEMPLATE
.PHONY: $(1)
$(1): GIT_VERSION $(2) builder-info
    $(CONTAINER_ENGINE) buildx build -f $(2) \
        $(DOCKER_BUILD_FLAGS) $(DOCKER_FLAGS) \
        --build-arg MODIFIERS="NOSTRIP=$${NOSTRIP} NOOPT=${NOOPT} ..." \
        --target $(5) \
        -t $(IMAGE_REPOSITORY)/$(IMAGE_NAME):$(4) .
endef

# 이미지 빌드 타겟 정의 (매크로 호출)
$(eval $(call DOCKER_IMAGE_TEMPLATE,docker-cilium-image,images/cilium/Dockerfile,cilium,$(DOCKER_IMAGE_TAG),release))
$(eval $(call DOCKER_IMAGE_TEMPLATE,dev-docker-image,images/cilium/Dockerfile,cilium-dev,$(DOCKER_IMAGE_TAG),release))
$(eval $(call DOCKER_IMAGE_TEMPLATE,dev-docker-image-debug,images/cilium/Dockerfile,cilium-dev,$(DOCKER_IMAGE_TAG),debug))
$(eval $(call DOCKER_IMAGE_TEMPLATE,docker-hubble-relay-image,images/hubble-relay/Dockerfile,hubble-relay,$(DOCKER_IMAGE_TAG),release))
```

### 5.4 Docker 이미지 종류

| make 타겟 | 이미지 | Dockerfile |
|---|---|---|
| `docker-cilium-image` | cilium-agent | `images/cilium/Dockerfile` |
| `docker-operator-image` | cilium-operator | `images/operator/Dockerfile` |
| `docker-hubble-relay-image` | hubble-relay | `images/hubble-relay/Dockerfile` |
| `docker-clustermesh-apiserver-image` | clustermesh-apiserver | `images/clustermesh-apiserver/Dockerfile` |
| `docker-standalone-dns-proxy-image` | standalone-dns-proxy | `images/standalone-dns-proxy/Dockerfile` |
| `dev-docker-image-debug` | cilium-agent (debug) | `images/cilium/Dockerfile` (debug target) |

---

## 6. Helm 3 차트

### 6.1 차트 구조

**디렉토리**: `install/kubernetes/cilium/`

```
install/kubernetes/cilium/
  |-- Chart.yaml              # 차트 메타데이터 (v1.20.0-dev)
  |-- values.yaml             # 기본 설정값 (자동 생성됨)
  |-- values.yaml.tmpl        # 설정값 템플릿 (소스)
  |-- values.schema.json      # JSON Schema 검증
  |-- templates/
  |     |-- _helpers.tpl       # 공통 헬퍼 함수
  |     |-- _extensions.tpl    # 확장 헬퍼
  |     |-- cilium-agent/      # DaemonSet, ConfigMap 등
  |     |-- cilium-operator/   # Deployment
  |     |-- hubble-relay/      # Deployment
  |     |-- hubble-ui/         # Deployment
  |     |-- clustermesh-apiserver/
  |     |-- cilium-configmap.yaml
  |     |-- cilium-ca-secret.yaml
  |     |-- NOTES.txt          # 설치 후 안내 메시지
  |     |-- validate.yaml      # 설정 검증 Job
  |     |-- warnings.txt       # 경고 메시지
  |-- files/                   # 정적 리소스 파일
```

### 6.2 Chart.yaml

**파일 경로**: `install/kubernetes/cilium/Chart.yaml`

```yaml
apiVersion: v2
name: cilium
displayName: Cilium
version: 1.20.0-dev
appVersion: 1.20.0-dev
kubeVersion: ">= 1.21.0-0"
description: eBPF-based Networking, Security, and Observability
annotations:
  artifacthub.io/crds: |
    - kind: CiliumNetworkPolicy
      version: v2
    - kind: CiliumClusterwideNetworkPolicy
    - kind: CiliumEndpoint
    - kind: CiliumNode
    - kind: CiliumIdentity
    - kind: CiliumBGPClusterConfig
    - kind: CiliumLoadBalancerIPPool
    # ... 20+ CRD 종류
```

### 6.3 values.yaml (자동 생성)

`values.yaml` 파일은 `values.yaml.tmpl`에서 자동 생성된다:

```yaml
# File generated by install/kubernetes/Makefile; DO NOT EDIT.
debug:
  enabled: false
  verbose: ~
rbac:
  create: true
imagePullSecrets: []
# ... 수천 줄의 설정 옵션
```

### 6.4 Helm 설치 예시

```bash
# 기본 설치
helm install cilium install/kubernetes/cilium/ --namespace kube-system

# 커스텀 설정으로 설치
helm install cilium install/kubernetes/cilium/ \
  --namespace kube-system \
  --set hubble.enabled=true \
  --set hubble.relay.enabled=true \
  --set hubble.ui.enabled=true \
  --set ipam.mode=kubernetes
```

---

## 7. 코드 생성 - Protobuf

### 7.1 Protobuf 정의

Cilium의 Hubble 관측 시스템은 protobuf/gRPC를 사용하여 네트워크 플로우 데이터를 전송한다.

**Proto 파일들**:

| 파일 | 용도 |
|---|---|
| `api/v1/flow/flow.proto` | 네트워크 플로우 메시지 정의 |
| `api/v1/observer/observer.proto` | Observer gRPC 서비스 정의 |
| `api/v1/peer/peer.proto` | 피어 디스커버리 |
| `api/v1/relay/relay.proto` | Hubble Relay 통신 |

### 7.2 Flow 메시지 구조

**파일 경로**: `api/v1/flow/flow.proto`

```protobuf
syntax = "proto3";
package flow;
option go_package = "github.com/cilium/cilium/api/v1/flow";

message Flow {
    google.protobuf.Timestamp time = 1;
    string uuid = 34;
    Verdict verdict = 2;
    uint32 drop_reason = 3 [deprecated=true];
    AuthType auth_type = 35;
    Ethernet ethernet = 4;   // L2
    IP IP = 5;               // L3
    Layer4 l4 = 6;           // L4
    Endpoint source = 8;
    Endpoint destination = 9;
    FlowType Type = 10;
    string node_name = 11;
    Layer7 l7 = 15;
    TrafficDirection traffic_direction = 22;
    // ...
}
```

### 7.3 Observer gRPC 서비스

**파일 경로**: `api/v1/observer/observer.proto`

```protobuf
service Observer {
    rpc GetFlows(GetFlowsRequest) returns (stream GetFlowsResponse) {}
    rpc GetAgentEvents(GetAgentEventsRequest) returns (stream GetAgentEventsResponse) {}
    rpc GetDebugEvents(GetDebugEventsRequest) returns (stream GetDebugEventsResponse) {}
    rpc GetNodes(GetNodesRequest) returns (GetNodesResponse) {}
    rpc GetNamespaces(GetNamespacesRequest) returns (GetNamespacesResponse) {}
    rpc ServerStatus(ServerStatusRequest) returns (ServerStatusResponse) {}
}
```

### 7.4 Protobuf 컴파일 파이프라인

**파일 경로**: `api/v1/Makefile.protoc`

```makefile
HUBBLE_PROTO_SOURCES := \
    ./flow/flow.proto \
    ./peer/peer.proto \
    ./observer/observer.proto \
    ./relay/relay.proto

# 생성 대상: .pb.go + .pb.json.go
HUBBLE_GO_TARGETS := $(HUBBLE_PROTO_SOURCES:.proto=.pb.go) \
                     $(HUBBLE_PROTO_SOURCES:.proto=.pb.json.go)

# protoc 플러그인들
HUBBLE_PROTOC_PLUGINS := --plugin=$(GOPATH)/bin/protoc-gen-doc
HUBBLE_PROTOC_PLUGINS += --plugin=$(GOPATH)/bin/protoc-gen-go-grpc
HUBBLE_PROTOC_PLUGINS += --plugin=$(GOPATH)/bin/protoc-gen-go-json
HUBBLE_PROTOC_PLUGINS += --plugin=$(GOPATH)/bin/protoc-gen-go

# 컴파일 명령
all:
    for proto in $(HUBBLE_PROTO_SOURCES) ; do \
        $(PROTOC) $(HUBBLE_PROTOC_PLUGINS) -I $(HUBBLE_PROTO_PATH) \
            --go_out=paths=source_relative:. \
            --go-grpc_out=require_unimplemented_servers=false,paths=source_relative:. \
            --go-json_out=orig_name=true,paths=source_relative:. \
            $${proto}; \
    done
```

**protoc 실행은 cilium-builder 컨테이너 안에서 수행된다** (`api/v1/Makefile`):

```makefile
proto:
    $(CONTAINER_ENGINE) container run --rm \
        --volume $(VOLUME):/src \
        --user "$(shell id -u):$(shell id -g)" \
        $(CONTAINER_IMAGE) \
        make -C /src -f Makefile.protoc
```

### 7.5 생성되는 파일

```
api/v1/
  |-- flow/
  |     |-- flow.proto           # 소스 정의
  |     |-- flow.pb.go           # 생성: Go 구조체 + 직렬화
  |     |-- flow.pb.json.go      # 생성: JSON 직렬화
  |     |-- flow_grpc.pb.go      # 생성: gRPC 클라이언트/서버
  |-- observer/
  |     |-- observer.pb.go
  |     |-- observer.pb.json.go
  |     |-- observer_grpc.pb.go
  |-- peer/
  |-- relay/
```

---

## 8. 코드 생성 - go-swagger

### 8.1 OpenAPI 명세

Cilium의 REST API는 OpenAPI(Swagger) 2.0 명세로 정의된다.

**파일 경로**: `api/v1/openapi.yaml`

```yaml
swagger: '2.0'
info:
  title: Cilium API
  version: v1beta1
basePath: "/v1"
produces:
  - application/json
paths:
  "/healthz":
    get:
      summary: Get health of Cilium daemon
      tags: [daemon]
  "/endpoint":
    get:
      summary: Retrieves a list of endpoints
  "/policy":
    get:
      summary: Retrieve list of all policies
  # ...
```

### 8.2 코드 생성 명령

**루트 Makefile에서**:

```makefile
generate-api: api/v1/openapi.yaml
    # 서버 코드 생성
    $(SWAGGER) generate server -s server -a restapi \
        -t api/v1 \
        -f api/v1/openapi.yaml \
        --default-scheme=unix \
        -C api/v1/cilium-server.yml \
        -r hack/spdx-copyright-header.txt

    # 클라이언트 코드 생성
    $(SWAGGER) generate client -a restapi \
        -t api/v1 \
        -f api/v1/openapi.yaml \
        -C api/v1/cilium-client.yml \
        -r hack/spdx-copyright-header.txt

    # import 정리
    goimports -w ./api/v1/client ./api/v1/models ./api/v1/server
```

### 8.3 생성되는 코드 구조

```
api/v1/
  |-- openapi.yaml            # 소스 명세
  |-- cilium-server.yml       # 서버 코드 생성 설정
  |-- cilium-client.yml       # 클라이언트 코드 생성 설정
  |-- server/                  # 생성: HTTP 서버 프레임워크
  |     |-- configure_cilium_api.go
  |     |-- embedded_spec.go
  |-- client/                  # 생성: HTTP 클라이언트 라이브러리
  |     |-- daemon/
  |     |-- endpoint/
  |     |-- policy/
  |-- models/                  # 생성: 데이터 모델 구조체
  |     |-- endpoint.go
  |     |-- policy.go
```

### 8.4 추가 API 명세

Cilium은 여러 API 명세를 가지고 있다:

| make 타겟 | 명세 파일 | 용도 |
|---|---|---|
| `generate-api` | `api/v1/openapi.yaml` | cilium-agent API |
| `generate-health-api` | `api/v1/health/openapi.yaml` | 헬스 체크 API |
| `generate-operator-api` | `api/v1/operator/openapi.yaml` | operator API |
| `generate-kvstoremesh-api` | `api/v1/kvstoremesh/openapi.yaml` | kvstoremesh API |
| `generate-hubble-api` | `api/v1/flow/flow.proto` 등 | Hubble gRPC API |

---

## 9. 코드 생성 - deepcopy-gen

### 9.1 개요

Kubernetes 리소스 타입은 `runtime.Object` 인터페이스를 구현해야 하며, 이를 위해 `DeepCopyObject()` 메서드가 필요하다. Cilium은 `k8s.io/code-generator/cmd/deepcopy-gen`을 사용하여 이 메서드들을 자동 생성한다.

### 9.2 Go 타입 마커

**파일 경로**: `pkg/k8s/apis/cilium.io/v2/types.go`

```go
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:categories={cilium},singular="ciliumendpoint",...
type CiliumEndpoint struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Status EndpointStatus `json:"status,omitempty"`
}
```

`// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object` 마커가 deepcopy-gen에게 `DeepCopyObject()` 메서드를 생성하라고 지시한다.

### 9.3 생성된 코드

**파일 경로**: `pkg/k8s/apis/cilium.io/v2/zz_generated.deepcopy.go`

```go
//go:build !ignore_autogenerated

// Code generated by deepcopy-gen. DO NOT EDIT.

package v2

// DeepCopyInto is an autogenerated deepcopy function
func (in *BGPAdvertisement) DeepCopyInto(out *BGPAdvertisement) {
    *out = *in
    if in.Service != nil {
        in, out := &in.Service, &out.Service
        *out = new(BGPServiceOptions)
        (*in).DeepCopyInto(*out)
    }
    if in.Selector != nil {
        in, out := &in.Selector, &out.Selector
        *out = new(v1.LabelSelector)
        (*in).DeepCopyInto(*out)
    }
}

// DeepCopy creates a new BGPAdvertisement
func (in *BGPAdvertisement) DeepCopy() *BGPAdvertisement {
    if in == nil {
        return nil
    }
    out := new(BGPAdvertisement)
    in.DeepCopyInto(out)
    return out
}
```

### 9.4 deepcopy-gen의 동작 원리

1. Go 소스 파일에서 `+k8s:deepcopy-gen` 마커가 있는 타입 검색
2. 각 타입의 필드를 분석:
   - 기본 타입 (int, string): 단순 대입
   - 포인터 타입: `new(T)`로 할당 후 값 복사
   - 슬라이스: `make([]T, len(in))`로 새 슬라이스 생성 후 원소 복사
   - 맵: 새 맵 생성 후 키-값 쌍 복사
   - 중첩 구조체: 재귀적으로 `DeepCopyInto()` 호출
3. `zz_generated.deepcopy.go` 파일로 출력

### 9.5 deepequal-gen (Cilium 확장)

Cilium은 자체 `deepequal-gen` 도구도 사용한다:

**파일 경로**: `contrib/scripts/k8s-code-gen.sh`

```bash
# deepequal 코드 생성 (같은 패턴이지만 비교 함수 생성)
go tool github.com/cilium/deepequal-gen \
    --output-file zz_generated.deepequal.go \
    --go-header-file "${boilerplate}" \
    "${input_pkgs[@]}"
```

생성 파일: `zz_generated.deepequal.go`

---

## 10. 코드 생성 - controller-gen (CRD)

### 10.1 개요

`controller-gen`은 Go 구조체의 kubebuilder 마커를 읽어 Kubernetes CRD(Custom Resource Definition) YAML 매니페스트를 생성한다.

### 10.2 생성 스크립트

**파일 경로**: `contrib/scripts/k8s-manifests-gen.sh`

```bash
# CRD 생성 옵션
CRD_OPTIONS="crd:crdVersions=v1"

# 소스 경로 (Go 타입이 정의된 곳)
CRD_PATHS="pkg/k8s/apis/cilium.io/v2;pkg/k8s/apis/cilium.io/v2alpha1"

# controller-gen 실행
go tool sigs.k8s.io/controller-tools/cmd/controller-gen \
    ${CRD_OPTIONS} \
    paths="${CRD_PATHS}" \
    output:crd:artifacts:config="${TMPDIR}"

# CRD 유효성 검사
go run tools/crdcheck "${TMPDIR}"

# 생성된 CRD를 적절한 디렉토리로 이동
# v2 CRD
for file in ${CRDS_CILIUM_V2}; do
    mv "${TMPDIR}/cilium.io_${file}.yaml" \
       "pkg/k8s/apis/cilium.io/client/crds/v2/${file}.yaml"
done

# v2alpha1 CRD
for file in ${CRDS_CILIUM_V2ALPHA1}; do
    mv "${TMPDIR}/cilium.io_${file}.yaml" \
       "pkg/k8s/apis/cilium.io/client/crds/v2alpha1/${file}.yaml"
done
```

### 10.3 kubebuilder 마커 예시

```go
// +kubebuilder:resource:categories={cilium},singular="ciliumendpoint",
//   path="ciliumendpoints",scope="Namespaced",shortName={cep,ciliumep}
// +kubebuilder:printcolumn:JSONPath=".status.identity.id",
//   description="Security Identity",name="Security Identity",type=integer
// +kubebuilder:printcolumn:JSONPath=".status.state",
//   description="Endpoint current state",name="Endpoint State",type=string
// +kubebuilder:storageversion
// +kubebuilder:validation:Format=cidr
// +kubebuilder:validation:Optional
// +kubebuilder:validation:Required
```

### 10.4 생성된 CRD 목록

**v2 CRD** (`pkg/k8s/apis/cilium.io/client/crds/v2/`):

| CRD 파일 | 리소스 |
|---|---|
| `ciliumnetworkpolicies.yaml` | CiliumNetworkPolicy |
| `ciliumclusterwidenetworkpolicies.yaml` | CiliumClusterwideNetworkPolicy |
| `ciliumendpoints.yaml` | CiliumEndpoint |
| `ciliumnodes.yaml` | CiliumNode |
| `ciliumidentities.yaml` | CiliumIdentity |
| `ciliumegressgatewaypolicies.yaml` | CiliumEgressGatewayPolicy |
| `ciliumenvoyconfigs.yaml` | CiliumEnvoyConfig |
| `ciliumloadbalancerippools.yaml` | CiliumLoadBalancerIPPool |
| `ciliumbgpclusterconfigs.yaml` | CiliumBGPClusterConfig |
| ... | (16+ CRD) |

**v2alpha1 CRD** (`pkg/k8s/apis/cilium.io/client/crds/v2alpha1/`):

| CRD 파일 | 리소스 |
|---|---|
| `ciliumendpointslices.yaml` | CiliumEndpointSlice |
| `ciliuml2announcementpolicies.yaml` | CiliumL2AnnouncementPolicy |
| `ciliumpodippools.yaml` | CiliumPodIPPool |
| `ciliumgatewayclassconfigs.yaml` | CiliumGatewayClassConfig |

---

## 11. 코드 생성 - client-gen

### 11.1 개요

`client-gen`은 Kubernetes API 타입에 대한 타입 세이프(typed) Go 클라이언트를 자동 생성한다.

### 11.2 생성 스크립트

**파일 경로**: `contrib/scripts/k8s-code-gen.sh`

```bash
# k8s.io/code-generator의 코드 생성 함수 로드
source "${CODEGEN_PKG}/kube_codegen.sh"

# Slim K8s API에 대한 클라이언트 생성
kube::codegen::gen_client \
    "./pkg/k8s/slim/k8s/api" \
    --with-watch \
    --output-dir "${TMPDIR}/github.com/cilium/cilium/pkg/k8s/slim/k8s/client" \
    --output-pkg "github.com/cilium/cilium/pkg/k8s/slim/k8s/client" \
    --plural-exceptions ${PLURAL_EXCEPTIONS} \
    --boilerplate "${SCRIPT_ROOT}/hack/custom-boilerplate.go.txt"

# Cilium CRD에 대한 클라이언트 생성
kube::codegen::gen_client \
    "./pkg/k8s/apis" \
    --with-watch \
    --output-dir "${TMPDIR}/github.com/cilium/cilium/pkg/k8s/client" \
    --output-pkg "github.com/cilium/cilium/pkg/k8s/client" \
    --plural-exceptions ${PLURAL_EXCEPTIONS} \
    --boilerplate "${SCRIPT_ROOT}/hack/custom-boilerplate.go.txt"

# deepcopy와 기타 헬퍼 생성
kube::codegen::gen_helpers \
    --boilerplate "${SCRIPT_ROOT}/hack/custom-boilerplate.go.txt" \
    "$PWD/pkg"
```

### 11.3 생성되는 코드 구조

**파일 경로**: `pkg/k8s/client/clientset/versioned/clientset.go`

```go
// Code generated by client-gen. DO NOT EDIT.

package versioned

type Interface interface {
    Discovery() discovery.DiscoveryInterface
    CiliumV2() ciliumv2.CiliumV2Interface
    CiliumV2alpha1() ciliumv2alpha1.CiliumV2alpha1Interface
}

type Clientset struct {
    *discovery.DiscoveryClient
    ciliumV2       *ciliumv2.CiliumV2Client
    ciliumV2alpha1 *ciliumv2alpha1.CiliumV2alpha1Client
}
```

### 11.4 생성되는 전체 코드

```
pkg/k8s/client/
  |-- clientset/versioned/
  |     |-- clientset.go          # Clientset 인터페이스
  |     |-- typed/
  |     |     |-- cilium.io/v2/          # v2 리소스 클라이언트
  |     |     |-- cilium.io/v2alpha1/    # v2alpha1 리소스 클라이언트
  |     |-- fake/                 # 테스트용 가짜 클라이언트
  |     |-- scheme/               # 스키마 등록
  |-- informers/                  # 캐싱 인포머
  |-- listers/                    # 리스터 인터페이스
  |-- applyconfiguration/         # Apply 설정
```

---

## 12. CI/CD 파이프라인

### 12.1 GitHub Actions 워크플로우

**디렉토리**: `.github/workflows/`

Cilium은 **88개 이상의 GitHub Actions 워크플로우**를 사용한다.

### 12.2 워크플로우 분류

| 카테고리 | 워크플로우 예시 | 설명 |
|---|---|---|
| **이미지 빌드** | `build-images-ci.yaml` | PR 시 CI 이미지 빌드 |
| | `build-images-releases.yaml` | 릴리즈 이미지 빌드 |
| | `build-images-base.yaml` | 베이스 이미지 빌드 |
| **적합성 테스트** | `conformance-ginkgo.yaml` | Ginkgo E2E 테스트 |
| | `conformance-gateway-api.yaml` | Gateway API 적합성 |
| | `conformance-aks.yaml` | Azure AKS 테스트 |
| | `conformance-eks.yaml` | AWS EKS 테스트 |
| | `conformance-gke.yaml` | GCP GKE 테스트 |
| | `conformance-clustermesh.yaml` | ClusterMesh 테스트 |
| | `conformance-ingress.yaml` | Ingress 적합성 |
| **CI** | `build-go-caches.yaml` | Go 빌드 캐시 준비 |
| | `cilium-cli.yaml` | cilium CLI 테스트 |
| | `codeql.yaml` | CodeQL 보안 분석 |
| **자동화** | `auto-labeler.yaml` | PR 자동 레이블링 |
| | `auto-approve.yaml` | 자동 승인 |
| | `close-stale-issues.yaml` | 오래된 이슈 닫기 |
| **백포트** | `call-backport-label-updater.yaml` | 백포트 레이블 업데이트 |

### 12.3 이미지 빌드 워크플로우

**파일 경로**: `.github/workflows/build-images-ci.yaml`

```yaml
name: Image CI Build

on:
  pull_request_target:
    types: [opened, synchronize, reopened]
  push:
    branches: [main, ft/main/**]
  merge_group:
    types: [checks_requested]

jobs:
  build-and-push-prs:
    timeout-minutes: 45
    name: Build and Push Images
    # ...
```

### 12.4 E2E 테스트 워크플로우

**파일 경로**: `.github/workflows/conformance-ginkgo.yaml`

```yaml
name: Conformance Ginkgo (ci-ginkgo)

on:
  workflow_call:
    inputs:
      PR-number:
        description: "Pull request number."
        required: true
        type: string
      SHA:
        description: "SHA under test (head of the PR branch)."
        required: true
        type: string
```

---

## 13. 테스트 프레임워크

### 13.1 Go 유닛 테스트

```makefile
# 일반 테스트
GO_TEST = CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS)
GOTEST_BASE := -timeout 720s

# 특권 테스트 (root 필요)
tests-privileged:
    PRIVILEGED_TESTS=true PATH=$(PATH):$(ROOT_DIR)/bpf \
        $(GO_TEST) $(TEST_LDFLAGS) $(TESTPKGS) $(GOTEST_BASE)

# 통합 테스트 (etcd 필요)
integration-tests: start-kvstores
    INTEGRATION_TESTS=true $(GO_TEST) $(TEST_UNITTEST_LDFLAGS) $(TESTPKGS)
```

### 13.2 테스트 도구

| 도구 | 용도 |
|---|---|
| **Ginkgo** | BDD 스타일 E2E 테스트 프레임워크 |
| **Testify** | 어설션 라이브러리 (assert, require) |
| **Gomega** | 매처 라이브러리 (Ginkgo와 함께 사용) |
| **tparse** | Go 테스트 출력 포매터 |
| **go-junit-report** | JUnit XML 리포트 생성 |

### 13.3 BPF 통합 테스트

**디렉토리**: `bpf/tests/`

BPF 테스트는 실제 BPF 프로그램을 컴파일하고 커널에 로드하여 실행한다.

**파일 경로**: `bpf/tests/bpftest/bpf_test.go`

```go
package bpftests

// Go 테스트 프레임워크가 BPF 프로그램을 로드하고 실행
import (
    "github.com/cilium/ebpf"
    "github.com/cilium/ebpf/perf"
    "github.com/cilium/coverbee"  // BPF 코드 커버리지
)

var (
    testPath           = flag.String("bpf-test-path", "", "Path to the eBPF tests")
    testCoverageReport = flag.String("coverage-report", "", "Coverage report path")
)
```

**BPF 테스트 빌드** (`bpf/tests/Makefile`):

```makefile
# BPF 테스트 프로그램 컴파일
CLANG_FLAGS := $(FLAGS) $(shell $(GO) run pkg/datapath/loader/tools/clang_cflags.go)
CLANG_FLAGS += -MD -mcpu=v3

# scapy로 생성된 테스트 패킷 헤더
SCAPY_HDR := bpf/tests/output/gen_pkts.h

%.o: %.c $(LIB) $(SCAPY_HDR)
    ${CLANG} ${CLANG_FLAGS} -c $< -o $@
```

**BPF 테스트 실행**:

```makefile
# Makefile (루트)
run_bpf_tests:
    DOCKER_ARGS="--privileged -v /sys:/sys" RUN_AS_ROOT=1 \
    contrib/scripts/builder.sh \
        make -C bpf/tests/ run
```

### 13.4 테스트 출력 포매팅

```makefile
# tparse 사용 가능 시 자동 전환
GOTEST_FORMATTER ?= cat
ifneq ($(shell command -v tparse),)
    GOTEST_FORMATTER = tparse $(GOTEST_FORMATTER_FLAGS)
    GO_TEST_FLAGS += -json  # tparse는 JSON 출력 필요
endif

# CODEOWNERS 기반 테스트 소유자 추적
ifneq ($(LOG_CODEOWNERS),)
    GOTEST_FORMATTER = tee \
        >(go-junit-report -code-owners=$(CODEOWNERS_PATH) -out "$(JUNIT_PATH)") \
        >($(GO) run tools/testowners --code-owners=$(CODEOWNERS_PATH)) \
        >(tparse $(GOTEST_FORMATTER_FLAGS))
endif
```

---

## 14. 개발 도구 및 워크플로우

### 14.1 주요 make 타겟

| 명령어 | 설명 |
|---|---|
| `make build` | 전체 바이너리 빌드 |
| `make debug` | 디버그 모드 빌드 (NOOPT=1, NOSTRIP=1) |
| `make generate-apis` | 모든 API 코드 생성 (Swagger + Protobuf) |
| `make generate-api` | cilium-agent REST API 코드 생성 |
| `make generate-hubble-api` | Hubble gRPC API 코드 생성 |
| `make generate-k8s-api` | Kubernetes 클라이언트/deepcopy 코드 생성 |
| `make generate-bpf` | BPF 관련 Go 코드 생성 |
| `make manifests` | CRD YAML 매니페스트 생성 |
| `make docker-cilium-image` | cilium-agent Docker 이미지 빌드 |
| `make dev-docker-image` | 개발용 Docker 이미지 빌드 |
| `make dev-docker-image-debug` | 디버그 Docker 이미지 (delve 포함) |
| `make kind` | Kind 클러스터 생성 |
| `make kind-down` | Kind 클러스터 삭제 |
| `make tests-privileged` | 특권 테스트 실행 |
| `make integration-tests` | 통합 테스트 실행 |
| `make run_bpf_tests` | BPF 테스트 실행 |
| `make lint` | golangci-lint + custom-lint |
| `make precheck` | 빌드 전 검증 (포맷, 태그, 보안 등) |
| `make postcheck` | 빌드 후 검증 (문서 등) |
| `make gofmt` | Go 코드 포맷팅 |
| `make govet` | Go 정적 분석 |
| `make dev-doctor` | 개발 환경 점검 |

### 14.2 precheck 파이프라인

`make precheck`는 빌드 전에 다양한 코드 품질 검사를 실행한다:

```makefile
precheck:
    contrib/scripts/check-fmt.sh            # Go 포맷 검사
    contrib/scripts/check-log-newlines.sh   # 로그 메시지 개행 검사
    contrib/scripts/check-test-tags.sh      # 테스트 태그 검사
    contrib/scripts/lock-check.sh           # 락 사용 검사
    contrib/scripts/check-viper.sh          # Viper 설정 검사
    contrib/scripts/custom-vet-check.sh     # 커스텀 vet 검사
    contrib/scripts/check-time.sh           # time 패키지 사용 검사
    contrib/scripts/check-go-testdata.sh    # 테스트 데이터 검사
    contrib/scripts/check-source-info.sh    # 소스 정보 검사
    contrib/scripts/check-xfrmstate.sh      # XFRM 상태 검사
    contrib/scripts/check-legacy-header-guard.sh  # 레거시 헤더 가드 검사
    contrib/scripts/check-datapathconfig.sh  # 데이터경로 설정 검사
    contrib/scripts/check-fipsonly.sh       # FIPS 모드 검사
    $(GO) run ./tools/slogloggercheck .     # slog 로거 검사
```

### 14.3 코드 생성 전체 파이프라인

```
make generate-apis
  |-- generate-api             (Swagger -> REST server/client/models)
  |-- generate-health-api      (Swagger -> health server/client)
  |-- generate-hubble-api      (Protobuf -> gRPC Go 코드)
  |-- generate-operator-api    (Swagger -> operator server/client)
  |-- generate-kvstoremesh-api (Swagger -> kvstoremesh server/client)
  |-- generate-sdp-api         (Protobuf -> standalone DNS proxy)

make generate-k8s-api
  |-- go-to-protobuf           (K8s slim API -> protobuf)
  |-- kube::codegen::gen_client (client-gen -> typed clients)
  |-- kube::codegen::gen_helpers (deepcopy-gen -> deepcopy methods)
  |-- kube::codegen::deepequal_helpers (deepequal-gen -> deepequal methods)

make manifests
  |-- controller-gen           (kubebuilder markers -> CRD YAML)
  |-- crdcheck                 (CRD 유효성 검증)

make generate-bpf
  |-- dpgen                    (BPF objects -> config structs)
  |-- bpf2go                   (BPF objects -> Go skeletons)
```

### 14.4 tools/ 디렉토리

**디렉토리**: `tools/`

| 도구 | 설명 |
|---|---|
| `alignchecker` | Go/BPF 구조체 정렬 검사 |
| `dpgen` | BPF 데이터경로 설정 구조체 생성 |
| `crdcheck` | CRD YAML 유효성 검증 |
| `crdlistgen` | CRD 문서 목록 생성 |
| `dev-doctor` | 개발 환경 점검 도구 |
| `metricslint` | 메트릭 네이밍 규칙 검사 |
| `licensegen` | 의존성 라이선스 수집 |
| `slogloggercheck` | slog 로거 올바른 사용 검사 |
| `testowners` | CODEOWNERS 기반 테스트 소유자 추적 |

---

## 15. PoC 안내

`poc-18-build-codegen/` 디렉토리에서 빌드 시스템과 코드 생성의 핵심 메커니즘을 시뮬레이션하는 PoC를 실행할 수 있다.

```bash
cd EDU/poc-18-build-codegen
go run main.go
```

PoC에서 시뮬레이션하는 항목:
1. **Protobuf 유사 코드 생성**: 메시지 정의 -> Go 구조체 + Marshal/Unmarshal 메서드
2. **DeepCopy 코드 생성**: 구조체 분석 -> DeepCopy 메서드 자동 생성
3. **CRD YAML 생성**: Go 구조체 태그 -> OpenAPI 스키마 -> CRD YAML
4. **Makefile 타겟 의존성 그래프**: 위상 정렬 기반 빌드 순서 결정
5. **증분 빌드 감지**: 파일 수정 시간 기반 재빌드 판단

자세한 내용은 `poc-18-build-codegen/README.md`를 참조한다.
