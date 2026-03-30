# PoC-18: Build & Code Generation (빌드 및 코드 생성)

## 개요

Cilium의 코드 생성 도구 체계를 시뮬레이션한다. `tools/dpgen`의 config/maps 서브커맨드 패턴,
`deepcopy-gen`, `crdlistgen`, `api-flaggen`의 핵심 로직을 Go 표준 라이브러리만으로 재현한다.
eBPF 오브젝트에서 Go 코드를 자동 생성하는 파이프라인의 원리를 이해하는 것이 목적이다.

## 배경 지식

### 왜 Cilium의 빌드/코드생성 파이프라인이 복잡한가

Cilium은 커널 공간(eBPF/C)과 사용자 공간(Go) 사이에 양방향 데이터 교환이 필요하다.
C 구조체 레이아웃, 맵 정의, 설정값이 Go 코드와 **바이트 단위로 정확히** 일치해야 한다.
수동 관리는 오류 가능성이 높으므로, 자동 코드 생성으로 일관성을 보장한다.

### 주요 코드 생성 도구

- **dpgen (데이터패스 설정 생성)**: eBPF 오브젝트(.o)를 파싱하여 BTF(BPF Type Format) 정보를
  추출하고, Go 설정 struct와 맵 초기화 코드를 자동 생성한다. `go:generate` 디렉티브로 연동된다.
- **deepcopy-gen (Go 구조체 깊은 복사)**: Kubernetes CRD struct에 `DeepCopyInto`/`DeepCopy`
  메서드를 자동 생성한다. 슬라이스, 맵, 포인터 등 참조 타입을 재귀적으로 복사하여 타입 안전성을 보장한다.
- **BPF 컴파일 파이프라인**: C 소스 → clang/LLVM → eBPF 오브젝트(.o) → BTF 메타데이터 추출.
  BTF는 커널과 사용자 공간 간 타입 정보 공유의 핵심이다.

### 왜 코드 생성이 필요한가

1. **성능**: 런타임 리플렉션 대신 컴파일 타임에 타입이 결정되어 오버헤드가 없다
2. **타입 안전성**: C/eBPF 측 변경이 Go 측에 자동 반영되어 타입 불일치 버그를 방지한다
3. **일관성**: 수십 개의 eBPF 오브젝트에서 공유하는 맵/설정의 호환성을 빌드 시점에 검증한다

## 시뮬레이션하는 개념

| 컴포넌트 | 실제 Cilium 경로 | PoC 구현 | 핵심 역할 |
|----------|------------------|----------|----------|
| dpgen config | `tools/dpgen/config.go` | `varsToStruct`, `camelCase`, `btfVarGoType` | eBPF 변수 → Go 설정 struct |
| dpgen maps | `tools/dpgen/maps.go` | `renderMapSpecs`, `needMapSpec` | eBPF MapSpec → Go 맵 초기화 코드 |
| dpgen util | `tools/dpgen/util.go` | `bpfFlagsToString`, `mapSpecCompatible` | 플래그 변환, 호환성 검증 |
| deepcopy-gen | `zz_generated.deepcopy.go` | `renderDeepCopy` 템플릿 | CRD struct → DeepCopy 메서드 |
| crdlistgen | `tools/crdlistgen/main.go` | `cleanupCRDName`, `generateCRDList` | CRD 문서 목록(RST) 생성 |
| api-flaggen | `tools/api-flaggen/main.go` | `generateAPIFlagTable` | API 플래그 테이블 생성 |

## 아키텍처 다이어그램

### dpgen 파이프라인

```
  C 소스 (bpf_lxc.c, bpf_xdp.c ...)
       │
       ▼  clang/LLVM 컴파일
  eBPF 오브젝트 (.o)  ─── BTF 메타데이터 포함
       │
       ├─── config 서브커맨드 ──────────────────────────────┐
       │    LoadCollectionSpec                              │
       │       → Variables 순회                             │
       │          → .data.config 섹션 필터                  │
       │             → kind 태그 매칭 (node/object)         │
       │                → btfVarGoType (BTF Int → Go 타입)  │
       │                   → camelCase (snake → CamelCase)  │
       │                      → varsToStruct                │
       │                         → Go 설정 struct 생성      │
       │                                                    ▼
       │                                      config_generated.go
       │
       └─── maps 서브커맨드 ───────────────────────────────┐
            LoadCollectionSpec                              │
               → MapSpecs 순회                              │
                  → needMapSpec (Pinned 맵만 대상)          │
                     → mapSpecCompatible (오브젝트 간 검증) │
                        → BTF 키/값 타입 수집               │
                           → renderMapSpecs (템플릿)        │
                                                           ▼
                                          maps_generated.go + mapkv.btf
```

### 전체 코드 생성 도구 체계

```
  go:generate
       │
       ├── tools/dpgen config ──→ 설정 struct (Node, BPFLXC, BPFXDP ...)
       ├── tools/dpgen maps   ──→ 맵 초기화 코드 (maps_generated.go)
       ├── deepcopy-gen        ──→ DeepCopy 메서드 (zz_generated.deepcopy.go)
       ├── tools/crdlistgen    ──→ CRD 문서 목록 (Documentation/crdlist.rst)
       └── tools/api-flaggen   ──→ API 플래그 테이블 (RST 문서)
```

## 코드 해설

### 1. `camelCase` - 네이밍 변환 엔진

snake_case C 변수명을 Go 스타일 CamelCase로 변환한다. `stylized` 맵에 등록된
약어(BPF, IPv4, NAT, MAC 등)는 Go 컨벤션에 맞게 대문자로 유지된다.
예: `bpf_ipv4_nat` → `BPFIPv4NAT`, `endpoint_id` → `EndpointID`.
실제 `tools/dpgen/config.go`의 로직을 그대로 재현한다.

### 2. `btfVarGoType` - BTF 타입 변환

BTF(BPF Type Format)의 정수 타입 인코딩(Signed/Unsigned/Bool/Char)과
바이트 크기를 Go 타입 이름으로 매핑한다. `BTFSigned + 4byte → int32`,
`BTFUnsigned + 2byte → uint16`, `BTFBool → bool`. 실제 `config.go:btfVarGoType`의
switch 분기를 동일하게 구현한다.

### 3. `varsToStruct` - 설정 구조체 생성기

eBPF 변수 목록에서 지정된 섹션(`.data.config`)과 kind 태그를 필터링하고,
각 변수를 `btfVarGoType`과 `camelCase`로 변환하여 Go struct 코드를 렌더링한다.
doc comment, bpf 태그, 생성자 함수(`NewNode`)까지 포함한다.
실제 `tools/dpgen/config.go:varsToStruct`의 전체 파이프라인을 재현한다.

### 4. `renderMapSpecs` - 맵 코드 생성기

`text/template`을 사용하여 eBPF MapSpec을 Go 초기화 코드로 렌더링한다.
outer 맵(Pinned)과 inner 맵(맵 of 맵)을 분류하고, 이름순 정렬 후 템플릿에 주입한다.
실제 `tools/dpgen/maps.go`에서 `go:embed`로 `.tpl` 파일을 로드하는 패턴을 재현한다.

### 5. `mapSpecCompatible` - 호환성 검증

여러 eBPF 오브젝트(bpf_lxc.o, bpf_xdp.o 등)에서 같은 이름의 맵이 서로 다르게
정의되면 빌드를 실패시킨다. Type, KeySize, ValueSize, MaxEntries를 비교하여
런타임 맵 충돌을 사전에 방지한다.

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-18-build-codegen
go run main.go
```

### 예상 출력 (주요 부분 발췌)

```
=== Cilium Build & Code Generation 파이프라인 시뮬레이션 ===

[1] camelCase 변환: endpoint_id → EndpointID, bpf_ipv4_nat → BPFIPv4NAT
[2] btfVarGoType: signed 4byte → int32, unsigned 2byte → uint16, bool → bool
[3] varsToStruct → type Node struct { EndpointID uint16; IPv4Addr uint32; ... }
[4] MapSpec 생성: SKIP cilium_signals(PinNone), INCLUDE cilium_ipcache/cilium_lxc
[5] 호환성 검사: A vs B 호환 OK, A vs C 비호환 (value size 64 != 128)
[6] DeepCopy: *out = *in 후 맵/포인터/슬라이스 별도 복사 코드 생성
[7] CRD List: RST :ref: 링크 생성
[8] API Flag Table: tabwriter 정렬 테이블 생성
[9] go/format.Source: 생성된 코드 자동 포맷팅
```

## 핵심 포인트

1. **BTF 기반 타입 브릿지**: eBPF 오브젝트에 내장된 BTF 메타데이터가 C ↔ Go 타입 변환의
   신뢰할 수 있는 소스(single source of truth)이다. 수동 매핑이 아닌 자동 추출이다.

2. **빌드 시점 호환성 검증**: `mapSpecCompatible`은 여러 eBPF 프로그램이 공유하는 맵의
   스키마가 일치하는지 빌드 시점에 확인한다. 런타임 오류를 사전에 차단한다.

3. **go:generate 통합**: `dpgen`은 독립 바이너리가 아니라 `go:generate` 디렉티브로 호출된다.
   `go generate ./...` 한 번으로 모든 코드 생성이 수행된다.

4. **Pinning 기반 필터링**: `needMapSpec`은 `Pinning != PinNone`인 맵만 코드 생성 대상으로
   삼는다. Pinned 맵은 bpffs에 고정되어 프로그램 간 공유되므로 Go 측 관리가 필요하다.

5. **DeepCopy 패턴**: `*out = *in`으로 값 타입을 한 번에 복사한 뒤, 참조 타입(포인터/슬라이스/맵)만
   별도로 깊은 복사한다. 이 패턴은 성능과 정확성을 동시에 확보한다.

6. **text/template 활용**: Go 표준 라이브러리의 `text/template`에 커스텀 함수(`camelCase`,
   `bpfFlagsToString`)를 등록하여 코드 생성 템플릿을 구동한다.

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | PoC |
|------|------------|-----|
| BTF 파싱 | `cilium/ebpf` 라이브러리로 .o 파일의 BTF 섹션 직접 파싱 | BTFType 구조체로 시뮬레이션 |
| eBPF 오브젝트 로드 | `ebpf.LoadCollectionSpec`으로 ELF 바이너리 파싱 | 하드코딩된 테스트 데이터 |
| 템플릿 로드 | `go:embed`로 `.tpl` 파일을 바이너리에 내장 | 상수 문자열로 인라인 정의 |
| 코드 포맷팅 | `go/format.Source` + `goimports` 적용 | `go/format.Source`만 사용 |
| inner 맵 처리 | BTF에서 inner 맵의 키/값 타입을 추출하여 combined BTF blob 생성 | InnerMap 포인터로 단순화 |
| deepcopy-gen | `controller-gen` 또는 `deepcopy-gen` 외부 도구가 AST 파싱 | 수동 StructDef 정의 + 템플릿 |
| CRD 문서 생성 | `os.WalkDir`로 Documentation/ 순회하며 RST 파일 매칭 | 하드코딩된 CRD 목록 |
| 맵 플래그 | 전체 BPF 맵 플래그 상수 지원 | 3개 주요 플래그만 구현 |
| 에러 처리 | `slog` 로깅 + 상세한 에러 메시지 | `fmt.Errorf` 기본 에러 |

## 관련 문서

- [18-build-codegen.md](../18-build-codegen.md) - Cilium 빌드/코드생성 심화 문서
