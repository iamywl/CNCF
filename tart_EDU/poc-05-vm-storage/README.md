# PoC-05: Local/OCI 이중 스토리지 시뮬레이션

## 개요

tart의 VM 스토리지 아키텍처를 Go 표준 라이브러리만으로 재현한다.
tart는 로컬 VM과 OCI 레지스트리에서 Pull한 이미지를 이중 스토리지 구조로 관리하며,
OCI 캐시에서는 태그→다이제스트 심볼릭 링크와 참조 카운트 기반 GC를 사용한다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다. 임시 디렉토리에서 전체 스토리지 워크플로우를 시뮬레이션한다.

## 핵심 시뮬레이션 포인트

### 1. VMStorageLocal (tart VMStorageLocal.swift)
- `~/.tart/vms/{name}/` 디렉토리 구조
- VM 생성(Create), 열기(Open), 이름 변경(Rename), 삭제(Delete), 목록(List)
- `config.json` 파일 존재 여부로 초기화 상태 판단

### 2. VMStorageOCI (tart VMStorageOCI.swift)
- `~/.tart/cache/OCIs/{host}/{namespace}/{reference}` 구조
- 태그 이미지: 심볼릭 링크 (latest → sha256:abc...)
- 다이제스트 이미지: 실제 디렉토리
- `Link()`: 태그→다이제스트 심볼릭 링크 생성/갱신
- `Digest()`: 심볼릭 링크의 대상에서 다이제스트 추출

### 3. GC (가비지 컬렉션, tart gc() 메서드)
- 깨진 심볼릭 링크(대상 없음) 자동 삭제
- 참조 카운트 기반: 심볼릭 링크 개수 = 참조 수
- 참조 카운트 0이고 `ExplicitlyPulled`가 아닌 다이제스트 삭제
- `ExplicitlyPulled`: 다이제스트로 직접 Pull한 이미지는 GC 보호

### 4. VMStorageHelper (tart VMStorageHelper.swift)
- 이름 형식 자동 분석: `host/namespace:tag` → OCI, 단순 이름 → Local
- `RemoteName` 파싱: host, namespace, reference(tag/digest) 분리

## tart 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `Sources/tart/VMStorageLocal.swift` | VMStorageLocal: vms/ 디렉토리 기반 로컬 VM CRUD |
| `Sources/tart/VMStorageOCI.swift` | VMStorageOCI: 심볼릭 링크 캐시, GC, Pull/Link |
| `Sources/tart/VMStorageHelper.swift` | VMStorageHelper: 이름 → Local/OCI 라우팅 |
| `Sources/tart/VMDirectory.swift` | VMDirectory: VM 파일(디스크, NVRAM, 설정) 관리 |

## 아키텍처

```
~/.tart/
├── vms/                          ← VMStorageLocal
│   ├── macos-sonoma/
│   │   └── config.json
│   └── ubuntu-24/
│       └── config.json
│
└── cache/OCIs/                   ← VMStorageOCI
    └── ghcr.io/
        └── cirruslabs/
            └── macos-sonoma-base/
                ├── latest → sha256:9876...  (심볼릭 링크)
                └── sha256:9876.../          (실제 데이터)
                    └── config.json

GC 알고리즘:
  1) 깨진 심볼릭 링크 삭제
  2) refCounts[resolved_path] += (isSymlink ? 1 : 0)
  3) refCount==0 && !explicitlyPulled → 삭제
```
