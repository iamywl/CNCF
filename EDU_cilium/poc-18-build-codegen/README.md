# PoC 18: Cilium 빌드 시스템 및 코드 생성 시뮬레이션

## 개요

이 PoC는 Cilium 프로젝트의 빌드 시스템과 자동 코드 생성 파이프라인의 핵심 메커니즘을 순수 Go 표준 라이브러리만으로 시뮬레이션한다. 외부 의존성 없이 단일 파일로 구성되어 있다.

## 실행 방법

```bash
cd EDU/poc-18-build-codegen
go run main.go
```

## 시뮬레이션 항목

### 1. Protobuf 코드 생성

Cilium의 Hubble 관측 시스템에서 사용하는 protobuf 정의(`api/v1/flow/flow.proto`, `api/v1/observer/observer.proto`)를 protoc로 컴파일하는 과정을 시뮬레이션한다.

**실제 Cilium에서의 동작**:
- `api/v1/Makefile.protoc`에서 `protoc` + `protoc-gen-go`, `protoc-gen-go-grpc`, `protoc-gen-go-json` 플러그인을 사용
- `.proto` 파일에서 Go 구조체, Marshal/Unmarshal 메서드, gRPC 서버/클라이언트 인터페이스를 생성
- cilium-builder 컨테이너 내부에서 실행 (`make generate-hubble-api`)

**PoC에서의 시뮬레이션**:
- Proto 메시지/enum/서비스 정의를 Go 데이터 구조로 표현
- Go `text/template`을 사용하여 구조체, Marshal/Unmarshal, gRPC 인터페이스 코드 생성
- Cilium의 Flow, Endpoint, Verdict, Observer 서비스를 예시로 사용

### 2. DeepCopy 코드 생성

Kubernetes CRD 리소스 타입에 필요한 `DeepCopyObject()`, `DeepCopyInto()`, `DeepCopy()` 메서드를 자동 생성하는 과정을 시뮬레이션한다.

**실제 Cilium에서의 동작**:
- `contrib/scripts/k8s-code-gen.sh`에서 `k8s.io/code-generator`의 `deepcopy-gen` 사용
- `// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object` 마커가 있는 타입을 검색
- 필드 타입에 따라 적절한 복사 전략 적용 (기본 타입: 대입, 포인터: new+copy, 슬라이스: make+copy, 맵: make+iterate)
- `zz_generated.deepcopy.go` 파일로 출력

**PoC에서의 시뮬레이션**:
- 구조체 필드를 Primitive, Pointer, Slice, Map, Struct 종류로 분류
- 각 종류에 맞는 DeepCopy 코드를 템플릿으로 생성
- CiliumNetworkPolicy, CiliumNode 등 실제 Cilium CRD 타입을 예시로 사용
- `runtime.Object` 인터페이스를 위한 `DeepCopyObject()` 생성 포함

### 3. CRD YAML 생성

Go 구조체의 kubebuilder 마커를 읽어 Kubernetes CRD(Custom Resource Definition) YAML 매니페스트를 생성하는 `controller-gen`의 동작을 시뮬레이션한다.

**실제 Cilium에서의 동작**:
- `contrib/scripts/k8s-manifests-gen.sh`에서 `sigs.k8s.io/controller-tools/cmd/controller-gen` 사용
- `pkg/k8s/apis/cilium.io/v2/` 및 `v2alpha1/` 경로의 Go 타입에서 마커 추출
- `+kubebuilder:resource`, `+kubebuilder:printcolumn`, `+kubebuilder:validation` 등의 마커 처리
- `pkg/k8s/apis/cilium.io/client/crds/v2/` 및 `v2alpha1/`에 CRD YAML 출력

**PoC에서의 시뮬레이션**:
- CRD 메타데이터(group, version, kind, scope, shortNames, categories) 정의
- PrintColumn 정보 포함
- OpenAPI v3 스키마 속성 (type, description, format, items, properties) 생성
- CiliumNetworkPolicy CRD를 예시로 사용

### 4. Makefile 빌드 의존성 그래프

Cilium의 Makefile 타겟 의존성을 위상 정렬하여 빌드 순서를 결정하는 메커니즘을 시뮬레이션한다.

**실제 Cilium에서의 동작**:
- `Makefile`(루트)이 `Makefile.defs`, `Makefile.quiet`, `Makefile.docker`, `Makefile.kind`를 include
- `all` -> `precheck` -> `build` -> `postcheck` 순서로 실행
- `build`는 각 서브디렉토리(`daemon`, `operator`, `bpf` 등)에 재귀적으로 `make all` 호출
- BPF 프로그램은 `clang --target=bpf`로 컴파일, Go 바이너리는 `go build`로 빌드

**PoC에서의 시뮬레이션**:
- 빌드 타겟과 의존성을 그래프 자료구조로 표현
- 위상 정렬(topological sort)으로 빌드 순서 결정
- 순환 의존성 감지
- 특정 타겟 빌드에 필요한 의존성 체인 해결(resolution)
- 의존성 트리 시각화
- `all`, `build`, `generate-apis`, `docker-cilium-image` 등 실제 타겟 시뮬레이션

### 5. 증분 빌드 감지

Makefile의 파일 수정 시간 기반 재빌드 판단 로직을 시뮬레이션한다.

**실제 Cilium에서의 동작**:
- Make는 타겟 파일의 수정 시간과 의존성 파일들의 수정 시간을 비교
- 의존성 파일이 타겟보다 새로우면 타겟을 재빌드
- BPF 헤더(`bpf/include/`) 변경 시 모든 BPF 프로그램이 재컴파일됨
- `.proto` 파일 변경 시 `.pb.go` 재생성, `openapi.yaml` 변경 시 서버/클라이언트 재생성

**PoC에서의 시뮬레이션**:
- 파일 수정 시간 기반 재빌드 판단 로직 구현
- BPF 프로그램 빌드 시나리오: 헤더 수정 -> 관련 .o 파일 모두 재컴파일
- Go 바이너리 빌드 시나리오: openapi.yaml 수정 -> 모델 재생성 -> 바이너리 재빌드
- 빌드 캐스케이드(연쇄 반응) 시각화

## 관련 Cilium 파일

| 파일 | 설명 |
|---|---|
| `Makefile` | 루트 빌드 진입점 |
| `Makefile.defs` | 공통 변수, 플래그, 도구 정의 |
| `Makefile.quiet` | 빌드 출력 제어 (V=0/V=1) |
| `Makefile.docker` | Docker 이미지 빌드 템플릿 |
| `Makefile.kind` | Kind 클러스터 개발 환경 |
| `api/v1/Makefile.protoc` | Protobuf 컴파일 규칙 |
| `api/v1/flow/flow.proto` | Flow 메시지 정의 |
| `api/v1/observer/observer.proto` | Observer gRPC 서비스 정의 |
| `api/v1/openapi.yaml` | cilium-agent REST API 명세 |
| `bpf/Makefile` | BPF 프로그램 빌드 |
| `bpf/Makefile.bpf` | BPF 컴파일 플래그 |
| `images/cilium/Dockerfile` | 멀티스테이지 Docker 빌드 |
| `contrib/scripts/k8s-code-gen.sh` | K8s 코드 생성 (deepcopy, client, informer) |
| `contrib/scripts/k8s-manifests-gen.sh` | CRD YAML 생성 (controller-gen) |
| `pkg/k8s/apis/cilium.io/v2/types.go` | CRD Go 타입 정의 (kubebuilder 마커) |
| `pkg/k8s/apis/cilium.io/v2/zz_generated.deepcopy.go` | 생성된 DeepCopy 코드 |
| `pkg/k8s/client/clientset/versioned/clientset.go` | 생성된 typed K8s 클라이언트 |
| `install/kubernetes/cilium/Chart.yaml` | Helm 차트 메타데이터 |

## 출력 예시

프로그램은 다음 5개의 시뮬레이션 결과를 순서대로 출력한다:

1. Protobuf 코드 생성 결과 (Go 구조체, Marshal/Unmarshal, gRPC 인터페이스)
2. DeepCopy 코드 생성 결과 (DeepCopyInto, DeepCopy, DeepCopyObject)
3. CRD YAML 생성 결과 (OpenAPI v3 스키마 포함)
4. Makefile 의존성 그래프 분석 (트리 시각화 + 위상 정렬)
5. 증분 빌드 감지 결과 (재빌드/스킵 판단 + 캐스케이드 분석)

마지막에 Cilium 전체 코드 생성 파이프라인(17단계)의 요약 테이블을 출력한다.
