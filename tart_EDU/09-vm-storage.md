# 09. VM 스토리지 시스템 심화

## 1. 개요

Tart의 VM 스토리지 시스템은 macOS/Linux 가상 머신 이미지를 로컬 파일시스템과 OCI 레지스트리 캐시에서
관리하는 핵심 인프라다. **이중 스토리지 아키텍처**(Dual Storage Architecture)를 채택하여
사용자가 직접 생성/관리하는 로컬 VM과 원격 레지스트리에서 가져온 OCI 이미지를 독립적으로 관리한다.

이 시스템이 해결해야 할 핵심 과제는 다음과 같다:

1. **빠른 VM 클론**: CI/CD 환경에서 수십 개의 VM을 반복적으로 클론해야 한다
2. **디스크 공간 효율**: 수십 GB 규모의 VM 이미지를 효율적으로 관리해야 한다
3. **OCI 호환 이미지 캐시**: 레지스트리에서 Pull한 이미지를 로컬에 캐시해야 한다
4. **자동 정리**: 사용하지 않는 캐시를 자동으로 수거해야 한다
5. **동시성 안전**: 여러 Tart 프로세스가 동시에 스토리지에 접근할 수 있어야 한다

### 왜 이중 스토리지인가?

단일 스토리지로 로컬 VM과 OCI 캐시를 함께 관리하면 명명 체계가 충돌한다. 로컬 VM은
`my-macos-vm` 같은 단순한 이름을 사용하지만, OCI 이미지는 `ghcr.io/cirruslabs/macos:latest`
같은 완전한 레지스트리 경로를 포함한다. 또한 OCI 이미지는 태그→다이제스트 간접 참조 구조가
필요하지만 로컬 VM에는 불필요하다. 이를 분리함으로써 각 스토리지의 내부 구조를 독립적으로
최적화할 수 있다.

```
+------------------------------------------------------+
|                  VMStorageHelper                      |
|  (이름 판별: 로컬 vs 리모트 → 적절한 스토리지 라우팅)      |
+------------------------------------------------------+
         |                              |
         v                              v
+-------------------+        +---------------------+
|  VMStorageLocal   |        |   VMStorageOCI      |
|  ~/.tart/vms/     |        |  ~/.tart/cache/OCIs/|
+-------------------+        +---------------------+
         |                              |
         v                              v
+---------------------------------------------------+
|                   VMDirectory                      |
|  config.json + disk.img + nvram.bin + state.vzvmsave|
+---------------------------------------------------+
         |
         v
+---------------------------------------------------+
|              Prunable / PrunableStorage            |
|  통합 캐시 정리 인터페이스                             |
+---------------------------------------------------+
```

## 2. 디렉토리 레이아웃

### Config 클래스의 경로 구성

모든 스토리지 경로의 최상위 기준은 `Config` 구조체에서 결정된다.
소스 파일: `Sources/tart/Config.swift`

```swift
struct Config {
  let tartHomeDir: URL
  let tartCacheDir: URL
  let tartTmpDir: URL

  init() throws {
    var tartHomeDir: URL

    if let customTartHome = ProcessInfo.processInfo.environment["TART_HOME"] {
      tartHomeDir = URL(fileURLWithPath: customTartHome, isDirectory: true)
      try Self.validateTartHome(url: tartHomeDir)
    } else {
      tartHomeDir = FileManager.default
        .homeDirectoryForCurrentUser
        .appendingPathComponent(".tart", isDirectory: true)
    }
    self.tartHomeDir = tartHomeDir

    tartCacheDir = tartHomeDir.appendingPathComponent("cache", isDirectory: true)
    try FileManager.default.createDirectory(at: tartCacheDir, withIntermediateDirectories: true)

    tartTmpDir = tartHomeDir.appendingPathComponent("tmp", isDirectory: true)
    try FileManager.default.createDirectory(at: tartTmpDir, withIntermediateDirectories: true)
  }
}
```

**왜 TART_HOME 환경변수를 지원하는가?** CI/CD 환경에서는 기본 홈 디렉토리가 아닌 별도의
고속 스토리지(NVMe SSD 등)에 VM 이미지를 저장해야 할 때가 있다. AWS EC2 Mac 인스턴스에서
로컬 NVMe를 사용하는 경우가 대표적이다.

경로를 검증하는 `validateTartHome` 메서드는 경로의 각 상위 디렉토리를 순회하며 존재 여부를
확인하고, 없으면 생성을 시도한다. 이는 단순히 `createDirectory(withIntermediateDirectories: true)`를
호출하는 것보다 더 정밀한 에러 메시지를 제공하기 위함이다.

### 전체 디렉토리 트리

```
~/.tart/  (또는 $TART_HOME)
├── vms/                              # VMStorageLocal 영역
│   ├── my-macos-vm/                  # 로컬 VM 1
│   │   ├── config.json               # VM 설정 (CPU, 메모리, MAC 등)
│   │   ├── disk.img                  # 디스크 이미지 (수십 GB)
│   │   ├── nvram.bin                 # NVRAM (부트 설정)
│   │   ├── state.vzvmsave            # 일시중지 상태 (있을 수도 없을 수도)
│   │   └── control.sock             # 실행 중 제어 소켓
│   └── ci-runner/                    # 로컬 VM 2
│       ├── config.json
│       ├── disk.img
│       └── nvram.bin
│
├── cache/
│   └── OCIs/                         # VMStorageOCI 영역
│       └── ghcr.io/                  # 호스트별 디렉토리
│           └── cirruslabs/
│               └── macos/
│                   ├── latest → sha256:abc123...  # 태그 (심볼릭 링크)
│                   ├── ventura → sha256:abc123... # 또 다른 태그
│                   └── sha256:abc123.../          # 다이제스트 (실제 데이터)
│                       ├── config.json
│                       ├── disk.img
│                       ├── nvram.bin
│                       ├── manifest.json          # OCI 매니페스트
│                       └── .explicitly-pulled     # 다이제스트로 직접 Pull한 표시
│
└── tmp/                              # 임시 디렉토리 (작업 중 사용)
    ├── <UUID>/                       # VMDirectory.temporary()
    └── <MD5-hash>/                   # VMDirectory.temporaryDeterministic()
```

| 경로 | 관리 클래스 | 설명 |
|------|-----------|------|
| `~/.tart/vms/` | `VMStorageLocal` | 로컬 VM 저장소 |
| `~/.tart/cache/OCIs/` | `VMStorageOCI` | OCI 레지스트리 캐시 |
| `~/.tart/tmp/` | `Config` | 임시 작업 디렉토리 |

## 3. VMDirectory - VM 번들 구조

모든 VM은 하나의 디렉토리 = 하나의 번들로 표현된다. `VMDirectory` 구조체가 이 번들의
내부 구조를 정의한다.

소스 파일: `Sources/tart/VMDirectory.swift`

### 필수 파일과 URL 계산 프로퍼티

```swift
struct VMDirectory: Prunable {
  var baseURL: URL

  var configURL: URL {
    baseURL.appendingPathComponent("config.json")
  }
  var diskURL: URL {
    baseURL.appendingPathComponent("disk.img")
  }
  var nvramURL: URL {
    baseURL.appendingPathComponent("nvram.bin")
  }
  var stateURL: URL {
    baseURL.appendingPathComponent("state.vzvmsave")
  }
  var manifestURL: URL {
    baseURL.appendingPathComponent("manifest.json")
  }
  var controlSocketURL: URL {
    URL(fileURLWithPath: "control.sock", relativeTo: baseURL)
  }
  var explicitlyPulledMark: URL {
    baseURL.appendingPathComponent(".explicitly-pulled")
  }
}
```

| 파일 | 필수 여부 | 설명 |
|------|---------|------|
| `config.json` | 필수 | VM 하드웨어 설정 (CPU 수, 메모리 크기, MAC 주소, 디스플레이 등) |
| `disk.img` | 필수 | 가상 디스크 이미지 (raw 또는 ASIF 포맷) |
| `nvram.bin` | 필수 | NVRAM 데이터 (부트 설정, macOS 보안 정책 등) |
| `state.vzvmsave` | 선택 | VM 일시중지 상태 스냅샷 (macOS 14+) |
| `manifest.json` | OCI만 | OCI 이미지 매니페스트 (레이어 해시, 크기 정보) |
| `control.sock` | 실행 중만 | Unix 도메인 소켓 (Guest Agent 통신용) |
| `.explicitly-pulled` | OCI만 | 다이제스트로 직접 Pull했음을 표시하는 마커 파일 |

### 초기화 상태 검증

VM 번들이 유효한지 판단하는 기준은 3개의 필수 파일 존재 여부다:

```swift
var initialized: Bool {
  FileManager.default.fileExists(atPath: configURL.path) &&
    FileManager.default.fileExists(atPath: diskURL.path) &&
    FileManager.default.fileExists(atPath: nvramURL.path)
}
```

**왜 3개 파일만 검사하는가?** `state.vzvmsave`는 Suspended 상태에서만 존재하고,
`manifest.json`은 OCI 캐시에서만 존재하며, `control.sock`은 실행 중에만 존재한다.
따라서 VM 번들의 "최소 유효 조건"은 config + disk + nvram이다.

### VM 상태 머신

```swift
enum State: String {
  case Running = "running"
  case Suspended = "suspended"
  case Stopped = "stopped"
}

func state() throws -> State {
  if try running() {
    return State.Running
  } else if FileManager.default.fileExists(atPath: stateURL.path) {
    return State.Suspended
  } else {
    return State.Stopped
  }
}
```

```
                   tart run
    Stopped ─────────────────────► Running
       ▲                              │
       │                              │ tart suspend (SIGUSR1)
       │ tart stop (SIGINT/SIGKILL)   │
       │                              ▼
       │                          Suspended
       │                              │
       └──────────────────────────────┘
              tart stop (상태파일 삭제)
```

실행 상태(`Running`)는 PID 기반 잠금으로 판별한다:

```swift
func running() throws -> Bool {
  guard let lock = try? lock() else {
    return false
  }
  return try lock.pid() != 0
}

func lock() throws -> PIDLock {
  try PIDLock(lockURL: configURL)
}
```

`config.json`에 `fcntl(2)` 기반 파일 잠금을 설정하고, 잠금을 획득한 프로세스의 PID를
기록한다. 이 PID가 0이 아니면 VM이 실행 중이라고 판단한다. `try?`를 사용하는 이유는
`tart delete`와의 레이스 컨디션에서 ENOENT가 발생할 수 있기 때문이다.

### 임시 디렉토리 전략

VMDirectory는 두 가지 임시 디렉토리 생성 방식을 제공한다:

```swift
// 완전 임의 임시 디렉토리 (Create, Clone 등)
static func temporary() throws -> VMDirectory {
  let tmpDir = try Config().tartTmpDir.appendingPathComponent(UUID().uuidString)
  try FileManager.default.createDirectory(at: tmpDir, withIntermediateDirectories: false)
  return VMDirectory(baseURL: tmpDir)
}

// 결정적 임시 디렉토리 (Pull 등, 동일 이미지 재시도 시 같은 경로 사용)
static func temporaryDeterministic(key: String) throws -> VMDirectory {
  let keyData = Data(key.utf8)
  let hash = Insecure.MD5.hash(data: keyData)
  let hashString = hash.compactMap { String(format: "%02x", $0) }.joined()
  let tmpDir = try Config().tartTmpDir.appendingPathComponent(hashString)
  try FileManager.default.createDirectory(at: tmpDir, withIntermediateDirectories: true)
  return VMDirectory(baseURL: tmpDir)
}
```

**왜 결정적 임시 디렉토리가 필요한가?** Pull 작업이 중간에 실패하고 재시도할 때, 동일한
이미지 이름에 대해 같은 임시 경로를 사용하면 이전에 부분적으로 다운로드된 데이터를
활용하거나 정리하기 쉽다. MD5 해시를 사용하는 이유는 충돌 방지보다는 이미지 이름을
파일시스템에 안전한 고정 길이 문자열로 변환하기 위함이다.

### 클론과 APFS Copy-on-Write

```swift
func clone(to: VMDirectory, generateMAC: Bool) throws {
  try FileManager.default.copyItem(at: configURL, to: to.configURL)
  try FileManager.default.copyItem(at: nvramURL, to: to.nvramURL)
  try FileManager.default.copyItem(at: diskURL, to: to.diskURL)
  try? FileManager.default.copyItem(at: stateURL, to: to.stateURL)

  if generateMAC {
    try to.regenerateMACAddress()
  }
}
```

겉보기에는 단순한 파일 복사지만, **APFS(Apple File System)** 위에서 실행될 때
`FileManager.copyItem`은 자동으로 Copy-on-Write 클론을 수행한다. 즉, 50GB 디스크 이미지를
"복사"해도 실제로는 메타데이터만 복사되어 거의 즉시 완료된다. 이후 원본이나 클론에서
쓰기가 발생할 때만 해당 블록이 실제로 복제된다.

```
클론 직후:
  원본 disk.img ──┐
                   ├──► [APFS 블록들 공유]
  클론 disk.img ──┘

쓰기 발생 후:
  원본 disk.img ──► [블록 A] [블록 B-원본] [블록 C]
  클론 disk.img ──► [블록 A] [블록 B-수정] [블록 C]
                      (공유)   (분리됨)      (공유)
```

**왜 MAC 주소를 재생성하는가?** 같은 네트워크에 동일한 MAC 주소를 가진 두 VM이 존재하면
DHCP 충돌과 네트워크 오류가 발생한다. `generateMAC` 플래그는 이미 동일 MAC을 가진 VM이
실행 중인지 확인한 후 결정된다.

### 디스크 크기 관리

VMDirectory는 3가지 크기 측정 방식을 제공한다:

```swift
func sizeBytes() throws -> Int {           // 논리적 크기 (파일 표면 크기)
  try configURL.sizeBytes() + diskURL.sizeBytes() + nvramURL.sizeBytes()
}

func allocatedSizeBytes() throws -> Int {  // 실제 할당 크기 (APFS CoW 고려)
  try configURL.allocatedSizeBytes() + diskURL.allocatedSizeBytes() + nvramURL.allocatedSizeBytes()
}

func deduplicatedSizeBytes() throws -> Int { // 중복 제거 후 크기
  try configURL.deduplicatedSizeBytes() + diskURL.deduplicatedSizeBytes() + nvramURL.deduplicatedSizeBytes()
}
```

| 메서드 | 의미 | 용도 |
|--------|-----|------|
| `sizeBytes()` | 논리적 전체 크기 | `tart list`의 Disk 열 |
| `allocatedSizeBytes()` | 실제 점유 공간 | `tart list`의 Size 열, 프루닝 기준 |
| `deduplicatedSizeBytes()` | CoW 공유 제외 크기 | 실제 고유 데이터량 파악 |

## 4. VMStorageLocal - 로컬 VM 스토리지

소스 파일: `Sources/tart/VMStorageLocal.swift`

### 구조와 초기화

```swift
class VMStorageLocal: PrunableStorage {
  let baseURL: URL

  init() throws {
    baseURL = try Config().tartHomeDir.appendingPathComponent("vms", isDirectory: true)
  }

  private func vmURL(_ name: String) -> URL {
    baseURL.appendingPathComponent(name, isDirectory: true)
  }
}
```

로컬 VM의 이름은 단순 문자열이고, 파일시스템에서 `~/.tart/vms/{name}/` 디렉토리에 직접 매핑된다.

### CRUD 연산

```
┌─────────────────────────────────────────────────────┐
│              VMStorageLocal API                       │
├─────────────┬───────────────────────────────────────┤
│ exists()    │ VMDirectory(baseURL).initialized 확인   │
│ open()      │ validate + accessDate 갱신 + 반환       │
│ create()    │ VMDirectory.initialize() 호출           │
│ move()      │ replaceItemAt()으로 원자적 이동          │
│ rename()    │ replaceItemAt()으로 원자적 이름변경       │
│ delete()    │ VMDirectory.delete() (잠금 확인 후)      │
│ list()      │ 전체 VM 목록 반환                        │
└─────────────┴───────────────────────────────────────┘
```

핵심 메서드별 상세:

```swift
func open(_ name: String) throws -> VMDirectory {
  let vmDir = VMDirectory(baseURL: vmURL(name))
  try vmDir.validate(userFriendlyName: name)
  try vmDir.baseURL.updateAccessDate()    // 마지막 접근 시간 갱신 (프루닝 기준)
  return vmDir
}

func create(_ name: String, overwrite: Bool = false) throws -> VMDirectory {
  let vmDir = VMDirectory(baseURL: vmURL(name))
  try vmDir.initialize(overwrite: overwrite)
  return vmDir
}

func move(_ name: String, from: VMDirectory) throws {
  _ = try FileManager.default.createDirectory(at: baseURL, withIntermediateDirectories: true)
  _ = try FileManager.default.replaceItemAt(vmURL(name), withItemAt: from.baseURL)
}
```

**왜 `replaceItemAt`을 사용하는가?** 일반적인 `moveItem`은 대상이 이미 존재하면 실패하지만,
`replaceItemAt`은 원자적으로 교체한다. 임시 디렉토리에서 작업을 완료한 후 최종 위치로
이동할 때, 동시에 다른 프로세스가 같은 이름으로 VM을 만들고 있을 수 있으므로 원자적
교체가 안전하다.

### 목록 조회와 필터링

```swift
func list() throws -> [(String, VMDirectory)] {
  do {
    return try FileManager.default.contentsOfDirectory(
      at: baseURL,
      includingPropertiesForKeys: [.isDirectoryKey],
      options: .skipsSubdirectoryDescendants).compactMap { url in
      let vmDir = VMDirectory(baseURL: url)
      if !vmDir.initialized {
        return nil
      }
      return (vmDir.name, vmDir)
    }
  } catch {
    if error.isFileNotFound() {
      return []
    }
    throw error
  }
}
```

`vms/` 디렉토리가 아직 존재하지 않으면 빈 배열을 반환한다. 초기화되지 않은(필수 파일이
부족한) 디렉토리는 `compactMap`으로 필터링한다.

### MAC 주소 충돌 검사

```swift
func hasVMsWithMACAddress(macAddress: String) throws -> Bool {
  try list().contains { try $1.macAddress() == macAddress }
}
```

Clone 커맨드에서 이 메서드를 호출하여, 동일 MAC 주소를 가진 기존 VM이 있는지 확인한 후
필요하면 새 MAC을 생성한다.

## 5. VMStorageOCI - OCI 캐시 스토리지

소스 파일: `Sources/tart/VMStorageOCI.swift`

### 심볼릭 링크 기반 태그-다이제스트 매핑

OCI 스토리지의 핵심 설계는 **태그를 심볼릭 링크로, 다이제스트를 실제 디렉토리로** 표현하는 것이다.

```
~/.tart/cache/OCIs/
└── ghcr.io/cirruslabs/macos/
    ├── latest         → sha256:abc123...    (심볼릭 링크 = 태그)
    ├── ventura        → sha256:abc123...    (심볼릭 링크 = 태그)
    ├── sonoma         → sha256:def456...    (심볼릭 링크 = 태그)
    ├── sha256:abc123.../                     (실제 디렉토리 = 다이제스트)
    │   ├── config.json
    │   ├── disk.img
    │   ├── nvram.bin
    │   └── manifest.json
    └── sha256:def456.../
        ├── config.json
        ├── disk.img
        ├── nvram.bin
        └── manifest.json
```

**왜 심볼릭 링크인가?** OCI 레지스트리의 태그 시스템을 그대로 반영한다.
하나의 다이제스트(내용 해시)에 여러 태그가 가리킬 수 있고, 태그를 업데이트하면
새 다이제스트를 가리키도록 심볼릭 링크만 변경하면 된다. 실제 이미지 데이터를 복제할
필요가 없다.

### 경로 인코딩

OCI 이미지 이름에는 콜론(`:`)이 포함되는데, Swift의 `URL.appendingPathComponent()`가
`example.com:8080` 같은 경로에서 콜론을 처리할 때 버그가 있다. 이를 우회하기 위해
퍼센트 인코딩을 적용한다:

```swift
// Sources/tart/VMStorageOCI.swift
private func percentEncode(_ s: String) -> String {
  return s.addingPercentEncoding(
    withAllowedCharacters: CharacterSet(charactersIn: ":").inverted
  )!
}

extension URL {
  func appendingRemoteName(_ name: RemoteName) -> URL {
    var result: URL = self
    for pathComponent in (percentEncode(name.host) + "/" + name.namespace
                          + "/" + name.reference.value).split(separator: "/") {
      result = result.appendingPathComponent(String(pathComponent))
    }
    return result
  }
}
```

### 목록 조회와 이름 복원

OCI 스토리지의 `list()`는 로컬과 달리 3-튜플 `(이름, VMDirectory, 심볼릭링크 여부)`를
반환한다:

```swift
func list() throws -> [(String, VMDirectory, Bool)] {
  // ...
  for case let foundURL as URL in enumerator {
    let vmDir = VMDirectory(baseURL: foundURL)
    if !vmDir.initialized { continue }

    let parts = [foundURL.deletingLastPathComponent().relativePath,
                 foundURL.lastPathComponent]

    let isSymlink = try foundURL.resourceValues(
      forKeys: [.isSymbolicLinkKey]).isSymbolicLink!

    if isSymlink {
      name = parts.joined(separator: ":")    // 태그: host/namespace:tag
    } else {
      name = parts.joined(separator: "@")    // 다이제스트: host/namespace@sha256:...
    }

    name = percentDecode(name)
    result.append((name, vmDir, isSymlink))
  }
}
```

파일시스템 경로에서 OCI 이미지 이름을 복원할 때, 심볼릭 링크이면 `:`로, 실제 디렉토리이면
`@`로 결합한다. 이것은 OCI 이미지 참조의 표준 문법(`image:tag` vs `image@digest`)을
그대로 따른다.

### Pull 흐름 상세

Pull 연산은 OCI 스토리지에서 가장 복잡한 흐름이다:

```
┌─────────────────────────────────────────────────────────────┐
│                     VMStorageOCI.pull()                       │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  1. 매니페스트 Pull (registry.pullManifest)                    │
│     ↓                                                        │
│  2. 다이제스트 이름 계산 (Digest.hash(manifestData))            │
│     ↓                                                        │
│  3. 이미 캐시됐는지 확인 (exists + linked)                      │
│     ├── YES → 즉시 리턴                                       │
│     └── NO → 계속                                             │
│  4. 호스트 디렉토리 잠금 획득 (FileLock)                         │
│     ↓                                                        │
│  5. 결정적 임시 디렉토리 생성 (temporaryDeterministic)           │
│     ↓                                                        │
│  6. 캐시 공간 확보 시도 (Prune.reclaimIfNeeded)                 │
│     ↓                                                        │
│  7. 로컬 레이어 캐시 탐색 (chooseLocalLayerCache)               │
│     ↓                                                        │
│  8. 레이어 다운로드 (tmpVMDir.pullFromRegistry, 최대 5회 재시도) │
│     ↓                                                        │
│  9. 최종 위치로 이동 (move)                                     │
│     ↓                                                        │
│  10. 심볼릭 링크 생성 (link) + GC                               │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

핵심 코드:

```swift
func pull(_ name: RemoteName, registry: Registry,
          concurrency: UInt, deduplicate: Bool) async throws {
  // 1. 매니페스트 Pull
  let (manifest, manifestData) = try await registry.pullManifest(
    reference: name.reference.value)

  // 2. 다이제스트 이름 계산
  let digestName = RemoteName(
    host: name.host, namespace: name.namespace,
    reference: Reference(digest: Digest.hash(manifestData)))

  // 3. 이미 캐시됐는지 확인
  if exists(name) && exists(digestName) && linked(from: name, to: digestName) {
    defaultLogger.appendNewLine("\(digestName) image is already cached and linked!")
    return
  }

  // 4. 호스트 디렉토리 잠금 (동일 호스트에 대한 동시 Pull 방지)
  let lock = try FileLock(lockURL: hostDirectoryURL(digestName))
  let sucessfullyLocked = try lock.trylock()
  if !sucessfullyLocked {
    print("waiting for lock...")
    try lock.lock()
  }
  defer { try! lock.unlock() }

  // 5~8. 임시 디렉토리에서 다운로드
  if !exists(digestName) {
    let tmpVMDir = try VMDirectory.temporaryDeterministic(key: name.description)
    // ...
    try await retry(maxAttempts: 5) {
      let localLayerCache = try await chooseLocalLayerCache(name, manifest, registry)
      try await tmpVMDir.pullFromRegistry(
        registry: registry, manifest: manifest,
        concurrency: concurrency,
        localLayerCache: localLayerCache, deduplicate: deduplicate)
    } recoverFromFailure: { error in
      if error is URLError { return .retry }
      return .throw
    }
    try move(digestName, from: tmpVMDir)
  }

  // 10. 심볼릭 링크 생성
  if name != digestName {
    try link(from: name, to: digestName)
  } else {
    VMDirectory(baseURL: vmURL(name)).markExplicitlyPulled()
  }
}
```

**왜 호스트 단위로 잠금하는가?** 동일 레지스트리의 다른 이미지를 동시에 Pull하면
네트워크 대역폭을 과도하게 사용할 수 있다. 또한 같은 이미지를 여러 프로세스에서
동시에 Pull하면 중복 다운로드가 발생한다. 호스트 디렉토리에 잠금을 설정하면 이를 방지한다.

**왜 5번까지 재시도하는가?** OCI 레지스트리와의 통신은 네트워크 오류(`URLError`)로
실패할 수 있다. 일시적 네트워크 문제를 자동 복구하기 위해 최대 5회 재시도한다.

### 심볼릭 링크 관리

```swift
func link(from: RemoteName, to: RemoteName) throws {
  try? FileManager.default.removeItem(at: vmURL(from))
  try FileManager.default.createSymbolicLink(at: vmURL(from),
                                              withDestinationURL: vmURL(to))
  try gc()
}

func linked(from: RemoteName, to: RemoteName) -> Bool {
  do {
    let resolvedFrom = try FileManager.default.destinationOfSymbolicLink(
      atPath: vmURL(from).path)
    return resolvedFrom == vmURL(to).path
  } catch {
    return false
  }
}
```

링크 생성 후 즉시 GC를 수행한다. 태그가 새 다이제스트를 가리키게 되면, 이전 다이제스트에
대한 참조가 0이 될 수 있기 때문이다.

## 6. 가비지 컬렉션 (GC)

### OCI 스토리지 GC 알고리즘

```swift
func gc() throws {
  var refCounts = Dictionary<URL, UInt>()

  guard let enumerator = FileManager.default.enumerator(
    at: baseURL,
    includingPropertiesForKeys: [.isSymbolicLinkKey]) else {
    return
  }

  for case let foundURL as URL in enumerator {
    let isSymlink = try foundURL.resourceValues(
      forKeys: [.isSymbolicLinkKey]).isSymbolicLink!

    // 깨진 심볼릭 링크 제거
    if isSymlink && foundURL == foundURL.resolvingSymlinksInPath() {
      try FileManager.default.removeItem(at: foundURL)
      continue
    }

    let vmDir = VMDirectory(baseURL: foundURL.resolvingSymlinksInPath())
    if !vmDir.initialized { continue }

    // 참조 카운트: 심볼릭 링크가 가리키면 +1, 자기 자신(다이제스트)이면 +0
    refCounts[vmDir.baseURL] = (refCounts[vmDir.baseURL] ?? 0) + (isSymlink ? 1 : 0)
  }

  // 참조 카운트 0이고 명시적으로 Pull되지 않은 다이제스트 삭제
  for (baseURL, incRefCount) in refCounts {
    let vmDir = VMDirectory(baseURL: baseURL)
    if !vmDir.isExplicitlyPulled() && incRefCount == 0 {
      try FileManager.default.removeItem(at: baseURL)
    }
  }
}
```

```
GC 알고리즘 다이어그램:

1단계: 전체 순회하며 참조 카운트 계산
   latest ──(symlink)──► sha256:abc  → refCount[abc] += 1
   ventura ─(symlink)──► sha256:abc  → refCount[abc] += 1
   sha256:abc ──────────► sha256:abc  → refCount[abc] += 0  (자기 자신)
   sha256:def ──────────► sha256:def  → refCount[def] += 0  (자기 자신)

2단계: 수거 판단
   sha256:abc  refCount=2  → 유지 (태그가 가리킴)
   sha256:def  refCount=0  → .explicitly-pulled 확인
                             ├── 있음 → 유지
                             └── 없음 → 삭제!
```

**왜 `.explicitly-pulled` 마커가 필요한가?** 사용자가 태그 대신 다이제스트로 직접
Pull(`tart pull ghcr.io/org/repo@sha256:abc...`)하면 심볼릭 링크가 생성되지 않으므로
참조 카운트가 항상 0이다. 마커 파일이 없으면 Pull 직후 바로 GC에 의해 삭제될 것이다.

### 깨진 심볼릭 링크 처리

```swift
if isSymlink && foundURL == foundURL.resolvingSymlinksInPath() {
  try FileManager.default.removeItem(at: foundURL)
  continue
}
```

심볼릭 링크를 resolve했을 때 자기 자신이 나오면(즉, 대상이 삭제되어 resolve가 실패하면),
깨진 링크로 판단하고 제거한다.

### 임시 디렉토리 GC

`Config.gc()` 메서드는 `~/.tart/tmp/` 내의 사용하지 않는 임시 디렉토리를 정리한다:

```swift
func gc() throws {
  for entry in try FileManager.default.contentsOfDirectory(
    at: tartTmpDir, includingPropertiesForKeys: [], options: []) {
    let lock = try FileLock(lockURL: entry)
    if try !lock.trylock() {
      continue    // 잠금 중이면 사용 중이므로 건너뜀
    }
    try FileManager.default.removeItem(at: entry)
    try lock.unlock()
  }
}
```

이 GC는 `Root.main()`에서 **모든 커맨드 실행 전에** 호출된다(Pull과 Clone 제외).
Pull과 Clone이 제외되는 이유는 이들 커맨드가 직접 임시 디렉토리를 생성하고 잠금을
설정하므로, GC와의 레이스 컨디션을 방지하기 위함이다.

## 7. LocalLayerCache - mmap 기반 레이어 캐시

소스 파일: `Sources/tart/LocalLayerCache.swift`

### 왜 로컬 레이어 캐시가 필요한가?

OCI VM 이미지의 디스크 레이어는 수십 GB에 달한다. 같은 베이스 이미지의 서로 다른 버전을
Pull할 때, 변경되지 않은 레이어를 다시 다운로드하면 시간과 대역폭이 낭비된다. 이미 로컬에
캐시된 다른 이미지의 디스크에서 동일한 레이어 데이터를 직접 읽어오면 네트워크 전송을
건너뛸 수 있다.

### mmap 기반 디스크 접근

```swift
struct LocalLayerCache {
  struct DigestInfo {
    let range: Range<Data.Index>
    let compressedDigest: String
    let uncompressedContentDigest: String?
  }

  let name: String
  let deduplicatedBytes: UInt64
  let diskURL: URL

  private let mappedDisk: Data
  private var digestToRange: [String: DigestInfo] = [:]
  private var offsetToRange: [UInt64: DigestInfo] = [:]

  init?(_ name: String, _ deduplicatedBytes: UInt64,
        _ diskURL: URL, _ manifest: OCIManifest) throws {
    self.name = name
    self.deduplicatedBytes = deduplicatedBytes
    self.diskURL = diskURL

    // mmap(2)로 디스크 매핑 - 실제 메모리를 소비하지 않음
    self.mappedDisk = try Data(contentsOf: diskURL, options: [.alwaysMapped])

    // 매니페스트의 레이어 정보로 오프셋-다이제스트 매핑 구축
    var offset: UInt64 = 0
    for layer in manifest.layers.filter({ $0.mediaType == diskV2MediaType }) {
      guard let uncompressedSize = layer.uncompressedSize() else {
        return nil
      }
      let info = DigestInfo(
        range: Int(offset)..<Int(offset + uncompressedSize),
        compressedDigest: layer.digest,
        uncompressedContentDigest: layer.uncompressedContentDigest()!
      )
      self.digestToRange[layer.digest] = info
      self.offsetToRange[offset] = info
      offset += uncompressedSize
    }
  }
}
```

**왜 mmap인가?** 수십 GB 디스크 이미지를 메모리에 전부 로드하면 메모리 부족이 발생한다.
`Data(contentsOf:options:.alwaysMapped)`는 `mmap(2)` 시스템 콜을 사용하여 파일을
가상 메모리에 매핑만 하고, 실제로 접근하는 페이지만 물리 메모리로 로드한다. 이를 통해
필요한 레이어 범위만 효율적으로 읽을 수 있다.

### 이중 룩업 전략

```swift
func findInfo(digest: String, offsetHint: UInt64) -> DigestInfo? {
  // 오프셋 힌트로 먼저 탐색 (빈 레이어 등 다이제스트 충돌 대비)
  if let info = self.offsetToRange[offsetHint], info.compressedDigest == digest {
    return info
  }
  return self.digestToRange[digest]
}

func subdata(_ range: Range<Data.Index>) -> Data {
  return self.mappedDisk.subdata(in: range)
}
```

**왜 오프셋 힌트가 필요한가?** 동일한 다이제스트를 가진 레이어가 여러 개 존재할 수 있다
(예: 빈 레이어). 오프셋 힌트를 사용하면 정확한 레이어를 찾을 수 있다. 오프셋으로 먼저
찾고, 실패하면 다이제스트로 폴백한다.

### 최적 베이스 이미지 선택

```swift
func chooseLocalLayerCache(_ name: RemoteName, _ manifest: OCIManifest,
                           _ registry: Registry) async throws -> LocalLayerCache? {
  let target = Swift.Set(manifest.layers)

  let calculateDeduplicatedBytes = { (manifest: OCIManifest) -> UInt64 in
    target.intersection(manifest.layers).map({ UInt64($0.size) }).reduce(0, +)
  }

  var candidates: [(name: String, vmDir: VMDirectory,
                     manifest: OCIManifest, deduplicatedBytes: UInt64)] = []

  for (name, vmDir, isSymlink) in try list() {
    if isSymlink { continue }
    guard let manifestJSON = try? Data(contentsOf: vmDir.manifestURL) else { continue }
    guard let manifest = try? OCIManifest(fromJSON: manifestJSON) else { continue }
    candidates.append((name, vmDir, manifest, calculateDeduplicatedBytes(manifest)))
  }

  // 최소 1GB 이상 절약할 수 있는 후보 중 가장 많이 절약하는 것 선택
  let choosen = candidates.filter {
    $0.deduplicatedBytes > 1024 * 1024 * 1024
  }.max { left, right in
    return left.deduplicatedBytes < right.deduplicatedBytes
  }

  return try choosen.flatMap({
    try LocalLayerCache($0.name, $0.deduplicatedBytes, $0.vmDir.diskURL, $0.manifest)
  })
}
```

```
베이스 이미지 선택 알고리즘:

Pull 대상: macOS 15.1 (레이어 A, B, C, D, E)

캐시된 이미지 1: macOS 15.0 (레이어 A, B, C, D, F)
  → 교집합: {A, B, C, D} = 40GB 절약

캐시된 이미지 2: macOS 14.0 (레이어 A, B, G, H, I)
  → 교집합: {A, B} = 15GB 절약

캐시된 이미지 3: Ubuntu (레이어 X, Y, Z)
  → 교집합: {} = 0 절약

최소 기준: 1GB 이상 절약 가능해야 함
선택: 이미지 1 (40GB 절약, 최대값)
```

## 8. Prunable/PrunableStorage 프로토콜

소스 파일: `Sources/tart/Prunable.swift`

```swift
protocol PrunableStorage {
  func prunables() throws -> [Prunable]
}

protocol Prunable {
  var url: URL { get }
  func delete() throws
  func accessDate() throws -> Date
  func sizeBytes() throws -> Int
  func allocatedSizeBytes() throws -> Int
}
```

`VMDirectory`가 `Prunable`을 준수하고, `VMStorageLocal`과 `VMStorageOCI`가
`PrunableStorage`를 준수한다. `Prune` 커맨드는 이 프로토콜을 통해 통합된 인터페이스로
캐시를 정리한다.

| 프루닝 전략 | 옵션 | 동작 |
|------------|------|------|
| 시간 기반 | `--older-than=N` | 마지막 접근이 N일 이전인 항목 삭제 |
| 용량 기반 | `--space-budget=N` | 총 NGB를 초과하는 항목 중 가장 오래된 것부터 삭제 |
| 자동 프루닝 | `Prune.reclaimIfNeeded()` | Pull/Clone 시 디스크 공간 부족하면 자동 정리 |

### VMStorageOCI의 prunables 구현

```swift
func prunables() throws -> [Prunable] {
  try list().filter { (_, _, isSymlink) in !isSymlink }.map { (_, vmDir, _) in vmDir }
}
```

**왜 심볼릭 링크를 제외하는가?** 심볼릭 링크(태그)를 삭제하면 해당 태그로의 접근이
불가능해지지만, 실제 데이터(다이제스트 디렉토리)는 남아 있어 공간이 회수되지 않는다.
프루닝의 목적은 공간 회수이므로 실제 데이터(비-심볼릭 링크 항목)만 프루닝 대상으로 삼는다.

## 9. VMStorageHelper - 통합 접근 레이어

소스 파일: `Sources/tart/VMStorageHelper.swift`

```swift
class VMStorageHelper {
  static func open(_ name: String) throws -> VMDirectory {
    try missingVMWrap(name) {
      if let remoteName = try? RemoteName(name) {
        return try VMStorageOCI().open(remoteName)
      } else {
        return try VMStorageLocal().open(name)
      }
    }
  }

  static func delete(_ name: String) throws {
    try missingVMWrap(name) {
      if let remoteName = try? RemoteName(name) {
        try VMStorageOCI().delete(remoteName)
      } else {
        try VMStorageLocal().delete(name)
      }
    }
  }
}
```

이름에 `/`가 포함되면(`RemoteName` 파싱 성공) OCI 스토리지로, 아니면 로컬 스토리지로
라우팅한다. `missingVMWrap`은 `ENOENT` 에러를 사용자 친화적인 "VM이 존재하지 않습니다"
메시지로 변환한다.

## 10. 동시성과 잠금

### 잠금 계층 구조

```
┌──────────────────────────────────────────────────┐
│                   잠금 계층                        │
├──────────────────────────────────────────────────┤
│                                                   │
│  1. tartHomeDir 전역 잠금                          │
│     └─ Clone에서 MAC 충돌 검사 시                   │
│                                                   │
│  2. 호스트 디렉토리 잠금                             │
│     └─ OCI Pull에서 동일 호스트 동시 Pull 방지       │
│                                                   │
│  3. config.json PID 잠금                           │
│     └─ VM 실행 중 다른 프로세스의 삭제/수정 방지      │
│                                                   │
│  4. 임시 디렉토리 잠금                               │
│     └─ GC에서 사용 중인 임시 디렉토리 보호            │
│                                                   │
└──────────────────────────────────────────────────┘
```

**왜 fcntl(2) 기반 잠금인가?** macOS에서 `flock(2)`은 NFS에서 제대로 동작하지 않을 수 있고,
`fcntl(2)` 기반 잠금은 프로세스가 종료되면 자동으로 해제된다. CI/CD 환경에서 Tart 프로세스가
비정상 종료되어도 잠금이 남아 있지 않아 수동 정리가 필요 없다.

## 11. 전체 데이터 흐름

### tart clone (로컬 → 로컬)

```
tart clone my-base my-worker

1. VMStorageHelper.open("my-base")
   → VMStorageLocal.open("my-base")
   → ~/.tart/vms/my-base/ 열기

2. VMDirectory.temporary()
   → ~/.tart/tmp/<UUID>/ 생성

3. 전역 잠금 획득 (tartHomeDir)

4. MAC 충돌 검사 + 클론 실행
   → copyItem(config.json)  → 메타데이터 복사
   → copyItem(nvram.bin)    → 메타데이터 복사
   → copyItem(disk.img)     → APFS CoW 클론 (즉시 완료)
   → MAC 재생성 (필요시)

5. VMStorageLocal.move("my-worker", from: tmpDir)
   → replaceItemAt()으로 원자적 이동

6. 전역 잠금 해제

7. 미할당 공간 확보 (Prune.reclaimIfNeeded)
```

### tart clone (OCI → 로컬)

```
tart clone ghcr.io/cirruslabs/macos:latest my-worker

1. RemoteName 파싱 → OCI 이름 인식

2. VMStorageOCI.exists(remoteName) → false
   → Registry.pullManifest() 호출
   → VMStorageOCI.pull() 수행 (섹션 5 참조)

3. VMStorageHelper.open("ghcr.io/cirruslabs/macos:latest")
   → VMStorageOCI.open(remoteName)
   → ~/.tart/cache/OCIs/ghcr.io/cirruslabs/macos/latest → sha256:... 따라감

4. APFS CoW 클론
   → OCI 캐시 → 로컬 VM으로 복사

5. VMStorageLocal.move("my-worker", from: tmpDir)
```

## 12. 디스크 포맷과 크기 조정

VMDirectory는 두 가지 디스크 포맷을 지원한다:

```swift
func resizeDisk(_ sizeGB: UInt16, format: DiskImageFormat = .raw) throws {
  let diskExists = FileManager.default.fileExists(atPath: diskURL.path)
  if diskExists {
    try resizeExistingDisk(sizeGB)
  } else {
    try createDisk(sizeGB: sizeGB, format: format)
  }
}

private func resizeRawDisk(_ sizeGB: UInt16) throws {
  let diskFileHandle = try FileHandle.init(forWritingTo: diskURL)
  let currentDiskFileLength = try diskFileHandle.seekToEnd()
  let desiredDiskFileLength = UInt64(sizeGB) * 1000 * 1000 * 1000

  if desiredDiskFileLength < currentDiskFileLength {
    throw RuntimeError.InvalidDiskSize("...")  // 축소 불가
  } else if desiredDiskFileLength > currentDiskFileLength {
    try diskFileHandle.truncate(atOffset: desiredDiskFileLength)
  }
}
```

| 포맷 | 설명 | 크기 조정 방식 |
|------|------|-------------|
| `.raw` | 전통적인 Raw 디스크 이미지 | `truncate()`로 확장 (sparse file) |
| `.asif` | Apple Sparse Image Format | `diskutil image resize`로 확장 |

**왜 축소를 허용하지 않는가?** 파일시스템이 이미 디스크 전체를 사용하고 있을 수 있다.
축소하면 데이터 손실이 발생할 수 있으므로 확장만 허용한다.

## 13. 설계 결정 요약

| 설계 결정 | 선택 | 이유 |
|----------|------|------|
| 이중 스토리지 | Local + OCI 분리 | 명명 체계 충돌 방지, 독립 최적화 |
| 태그→다이제스트 | 심볼릭 링크 | 원자적 업데이트, 다중 태그 공유 |
| VM 번들 | 디렉토리 = VM | 단순한 파일 복사로 클론 가능 |
| GC | 참조 카운트 | 사용하지 않는 다이제스트 자동 수거 |
| 클론 | APFS CoW | 수십 GB 디스크를 즉시 클론 |
| 레이어 캐시 | mmap | 메모리 부담 없이 수십 GB 디스크 접근 |
| 잠금 | fcntl(2) | 프로세스 종료 시 자동 해제 |
| 임시 디렉토리 | 결정적 + 임의 | Pull 재시도 안정성 + 일반 작업 격리 |
| 디스크 크기 | 확장만 허용 | 데이터 손실 방지 |
| 경로 인코딩 | 퍼센트 인코딩 | Swift URL 콜론 버그 우회 |

## 14. 소스 파일 참조

| 파일 | 경로 | 역할 |
|------|------|------|
| Config.swift | `Sources/tart/Config.swift` | TART_HOME, 캐시/임시 디렉토리 경로 |
| VMDirectory.swift | `Sources/tart/VMDirectory.swift` | VM 번들 구조, 상태 관리, 클론, 잠금 |
| VMStorageLocal.swift | `Sources/tart/VMStorageLocal.swift` | 로컬 VM CRUD |
| VMStorageOCI.swift | `Sources/tart/VMStorageOCI.swift` | OCI 캐시, Pull, 심볼릭 링크, GC |
| VMStorageHelper.swift | `Sources/tart/VMStorageHelper.swift` | 통합 접근 레이어 |
| LocalLayerCache.swift | `Sources/tart/LocalLayerCache.swift` | mmap 기반 레이어 재활용 |
| Prunable.swift | `Sources/tart/Prunable.swift` | 프루닝 프로토콜 정의 |
| Prune.swift | `Sources/tart/Commands/Prune.swift` | 프루닝 커맨드, 자동 프루닝 |
