# PoC-18: Build & Code Generation (빌드 및 코드 생성)

## 개요

Cilium의 코드 생성 도구 체계를 시뮬레이션한다.
`tools/dpgen`의 config/maps 서브커맨드 패턴, `deepcopy-gen`,
`crdlistgen`, `api-flaggen`의 핵심 로직을 재현한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 Cilium 경로 | PoC 구현 |
|----------|------------------|----------|
| dpgen config | `tools/dpgen/config.go` | varsToStruct, camelCase, btfVarGoType, sentencify |
| dpgen maps | `tools/dpgen/maps.go` | renderMapSpecs, needMapSpec, mapSpecCompatible |
| dpgen util | `tools/dpgen/util.go` | bpfFlagsToString, mapSpecByName |
| deepcopy-gen | `pkg/k8s/apis/.../zz_generated.deepcopy.go` | DeepCopyInto/DeepCopy 패턴 생성 |
| crdlistgen | `tools/crdlistgen/main.go` | cleanupCRDName, RST 문서 생성 |
| api-flaggen | `tools/api-flaggen/main.go` | API 플래그 테이블 (tabwriter) |

## 핵심 개념

### dpgen - eBPF 오브젝트에서 Go 코드 생성

1. **config 서브커맨드**: eBPF 오브젝트의 `.data.config` 섹션 변수를 Go struct로 변환
   - `btfVarGoType`: BTF Int 타입 → Go 타입 (`uint32`, `int16`, `bool` 등)
   - `camelCase`: snake_case → CamelCase (`bpf_ipv4_nat` → `BPFIPv4NAT`)
   - `stylized`: Go 스타일 약어 매핑 (BPF, IPv4, MAC, MTU 등)
   - `varsToStruct`: 필드 수집 → struct + 생성자 렌더링
   - `sentencify`: 첫 글자 대문자 + 마침표

2. **maps 서브커맨드**: eBPF MapSpec을 Go 생성자 코드로 변환
   - `needMapSpec`: Pinning != PinNone인 맵만 대상
   - `mapSpecCompatible`: 여러 오브젝트에서 같은 맵의 호환성 검증
   - `renderMapSpecs`: text/template으로 Go 코드 렌더링
   - `bpfFlagsToString`: 맵 플래그를 `BPF_F_NO_PREALLOC` 등으로 변환
   - 출력: `maps_generated.go` + `mapkv.btf`

3. **go:generate 연동**
   ```go
   //go:generate go run github.com/cilium/cilium/tools/dpgen config --path bpf_lxc.o --kind node --name Node
   //go:generate go run github.com/cilium/cilium/tools/dpgen maps ../../../bpf/bpf_*.o
   ```

### deepcopy-gen - DeepCopy 메서드 자동 생성
- `DeepCopyInto`: `*out = *in`으로 시작, 참조 타입만 별도 복사
- `DeepCopy`: `new(T)` 후 `DeepCopyInto` 호출
- 포인터: `*out = new(T); **out = **in`
- 슬라이스: `make + copy`
- 맵: `make + range copy`

### crdlistgen - CRD 문서 목록 생성
- `cleanupCRDName`: CRD 이름에서 버전 제거
- `WalkDir`: Documentation .rst 파일 순회
- 매칭 시 `:ref:` 링크 생성

## 실행

```bash
go run main.go
```

## dpgen 파이프라인

```
eBPF 오브젝트 (.o)
  │
  ├── config 서브커맨드
  │   └─ LoadCollectionSpec → Variables 순회
  │       └─ .data.config 섹션 필터
  │           └─ kind 태그 매칭
  │               └─ btfVarGoType + camelCase
  │                   └─ varsToStruct → Go struct + 생성자
  │
  └── maps 서브커맨드
      └─ LoadCollectionSpec → MapSpecs 순회
          └─ needMapSpec (Pinned만)
              └─ mapSpecCompatible (오브젝트 간 검증)
                  └─ BTF 키/값 수집 → combined BTF blob
                      └─ renderMapSpecs → maps_generated.go
```

## 관련 문서

- [18-build-codegen.md](../18-build-codegen.md) - Cilium 빌드/코드생성 심화 문서
