# 04. Tart 코드 구조

## 1. 프로젝트 레이아웃

```
tart/
├── Package.swift                    # Swift Package Manager 매니페스트
├── Sources/
│   └── tart/                        # 메인 실행 타겟
│       ├── Root.swift               # @main 진입점, 서브커맨드 등록
│       ├── VM.swift                 # VZVirtualMachine 래퍼 클래스
│       ├── VMConfig.swift           # VM 설정 (Codable JSON)
│       ├── VMDirectory.swift        # VM 번들 디렉토리 관리
│       ├── VMDirectory+OCI.swift    # OCI Pull/Push 확장
│       ├── VMDirectory+Archive.swift # Apple Archive 내보내기/가져오기
│       ├── VMStorageLocal.swift     # 로컬 VM 저장소 (~/.tart/vms/)
│       ├── VMStorageOCI.swift       # OCI 캐시 저장소 (~/.tart/cache/OCIs/)
│       ├── Config.swift             # 전역 설정 (TART_HOME, 임시 디렉토리)
│       ├── Fetcher.swift            # URLSession 비동기 HTTP 클라이언트
│       ├── FileLock.swift           # flock() 기반 파일 잠금
│       ├── PIDLock.swift            # fcntl() 기반 PID 잠금
│       ├── DiskImageFormat.swift    # RAW/ASIF 디스크 포맷
│       ├── Diskutil.swift           # diskutil 프로세스 래퍼
│       ├── Serial.swift             # 시리얼 포트 설정
│       ├── ControlSocket.swift      # VM 제어 Unix 소켓
│       ├── OTel.swift               # OpenTelemetry 초기화
│       ├── Utils.swift              # 유틸리티 함수
│       ├── Term.swift               # 터미널 raw 모드 제어
│       ├── IPSWCache.swift          # IPSW 캐시 관리
│       ├── LocalLayerCache.swift    # 로컬 레이어 캐시 (중복 제거)
│       ├── URL+Prunable.swift       # Prunable 프로토콜 URL 확장
│       ├── VM+Recovery.swift        # 복구 모드 확장
│       │
│       ├── Commands/                # CLI 서브커맨드 (18+개)
│       │   ├── Create.swift         # tart create --from-ipsw/--linux
│       │   ├── Clone.swift          # tart clone (로컬/리모트)
│       │   ├── Run.swift            # tart run (VM 실행, GUI 포함)
│       │   ├── Pull.swift           # tart pull (OCI 레지스트리)
│       │   ├── Push.swift           # tart push (OCI 레지스트리)
│       │   ├── Set.swift            # tart set (CPU/메모리/디스크 변경)
│       │   ├── Get.swift            # tart get (설정 조회)
│       │   ├── List.swift           # tart list (VM 목록)
│       │   ├── Delete.swift         # tart delete
│       │   ├── Stop.swift           # tart stop (실행 중 VM 중지)
│       │   ├── Suspend.swift        # tart suspend (macOS 14+)
│       │   ├── IP.swift             # tart ip (VM IP 주소 조회)
│       │   ├── Exec.swift           # tart exec (Guest Agent 명령 실행)
│       │   ├── Import.swift         # tart import (아카이브)
│       │   ├── Export.swift         # tart export (아카이브)
│       │   ├── Login.swift          # tart login (레지스트리 인증)
│       │   ├── Logout.swift         # tart logout
│       │   ├── Rename.swift         # tart rename
│       │   ├── Prune.swift          # tart prune (캐시 정리)
│       │   └── FQN.swift            # tart fqn (정규화된 이름)
│       │
│       ├── OCI/                     # OCI 레지스트리 프로토콜 구현
│       │   ├── Registry.swift       # HTTP 클라이언트, 인증, Blob/Manifest
│       │   ├── Manifest.swift       # OCIManifest, OCIConfig, OCIManifestLayer
│       │   ├── Digest.swift         # SHA256 다이제스트 계산
│       │   ├── RemoteName.swift     # host/namespace:tag@digest 파싱
│       │   ├── Authentication.swift # Authentication 프로토콜
│       │   ├── AuthenticationKeeper.swift # 토큰 캐시
│       │   ├── WWWAuthenticate.swift # WWW-Authenticate 헤더 파싱
│       │   ├── URL+Absolutize.swift # 상대 URL → 절대 URL
│       │   ├── Reference/           # ANTLR4 기반 OCI 참조 파서
│       │   │   └── Generated/       # ANTLR 자동생성 파서/렉서
│       │   └── Layerizer/           # 디스크 레이어 압축/해제
│       │       ├── Disk.swift       # Disk 프로토콜
│       │       └── DiskV2.swift     # LZ4 압축, 청크 분할, 병렬 Pull/Push
│       │
│       ├── Platform/                # 플랫폼 추상화
│       │   ├── Platform.swift       # Platform 프로토콜 정의
│       │   ├── Darwin.swift         # macOS (arm64) 플랫폼 구현
│       │   ├── Linux.swift          # Linux 플랫폼 구현
│       │   ├── OS.swift             # OS enum (darwin/linux)
│       │   └── Architecture.swift   # Architecture enum (arm64)
│       │
│       ├── Network/                 # 네트워크 추상화
│       │   ├── Network.swift        # Network 프로토콜
│       │   ├── NetworkShared.swift   # NAT (VZNATNetworkDeviceAttachment)
│       │   ├── NetworkBridged.swift  # 브리지 (VZBridgedNetworkDeviceAttachment)
│       │   └── Softnet.swift        # Softnet (소켓 페어 + 외부 프로세스)
│       │
│       ├── Credentials/             # 인증 정보 프로바이더
│       │   ├── CredentialsProvider.swift      # CredentialsProvider 프로토콜
│       │   ├── KeychainCredentialsProvider.swift # macOS Keychain
│       │   ├── DockerConfigCredentialsProvider.swift # ~/.docker/config.json
│       │   ├── EnvironmentCredentialsProvider.swift # 환경변수
│       │   └── StdinCredentials.swift          # 표준 입력
│       │
│       ├── VNC/                     # VNC 디스플레이
│       │   ├── VNC.swift            # VNC 프로토콜
│       │   ├── ScreenSharingVNC.swift # macOS 화면 공유 기반
│       │   └── FullFledgedVNC.swift   # 독립 VNC 서버
│       │
│       ├── CI/                      # CI/빌드 정보
│       │   └── CI.swift             # 버전 문자열
│       │
│       ├── Formatter/               # 출력 포맷터
│       │   └── Format.swift         # 테이블/JSON 출력
│       │
│       ├── Logging/                 # 로깅
│       │   └── (로거 구현)
│       │
│       ├── MACAddressResolver/      # MAC 주소 → IP 변환
│       │   ├── ARPCache.swift       # ARP 캐시 조회
│       │   ├── Lease.swift          # DHCP 리스 파싱
│       │   ├── Leases.swift         # 리스 파일 관리
│       │   ├── AgentResolver.swift  # Guest Agent 기반 해석
│       │   └── MACAddress.swift     # MAC 주소 유틸리티
│       │
│       ├── Passphrase/              # 패스프레이즈 생성
│       │   ├── PassphraseGenerator.swift
│       │   └── Words.swift          # 단어 사전
│       │
│       ├── DeviceInfo/              # 호스트 디바이스 정보
│       │   └── DeviceInfo.swift
│       │
│       └── ShellCompletions/        # 셸 자동완성
│           └── ShellCompletions.swift
│
├── Tests/
│   └── TartTests/                   # 유닛 테스트
│
├── benchmark/                       # 벤치마크 도구
│   ├── cmd/
│   └── internal/
│
├── integration-tests/               # 통합 테스트
│   └── tart/
│
├── docs/                            # Hugo 기반 문서 사이트
│   ├── blog/
│   ├── integrations/
│   └── theme/
│
├── scripts/                         # 빌드/배포 스크립트
├── .github/workflows/               # GitHub Actions CI/CD
└── .ci/                             # CI 관련 패키지
```

## 2. 빌드 시스템

### Swift Package Manager (Package.swift)

```
Package(
  name: "Tart",
  platforms: [.macOS(.v13)],       // 최소 macOS 13 (Ventura)
  products: [
    .executable(name: "tart", targets: ["tart"])
  ],
  dependencies: [...]               // 14개 외부 패키지
  targets: [
    .executableTarget(name: "tart", dependencies: [...]),
    .testTarget(name: "TartTests", dependencies: ["tart"])
  ]
)
```

### 주요 의존성

| 패키지 | 용도 | 버전 |
|--------|------|------|
| swift-argument-parser | CLI 파싱 | >= 1.6.1 |
| Dynamic | Obj-C 동적 호출 | master |
| swift-algorithms | 컬렉션 알고리즘 (chunks) | >= 1.2.0 |
| SwiftDate | 날짜 처리 | >= 7.0.0 |
| Antlr4 | OCI Reference 파서 | 4.13.2 (exact) |
| swift-atomics | 원자적 연산 | >= 1.2.0 |
| TextTable | 테이블 포맷 출력 | master |
| swift-sysctl | 시스템 정보 조회 | >= 1.8.0 |
| SwiftRadix | MAC 주소 변환 | >= 1.3.1 |
| Semaphore | 비동기 세마포어 | >= 0.0.8 |
| swift-retry | 재시도 로직 | >= 0.2.3 |
| swift-xattr | 확장 속성 | >= 3.0.0 |
| grpc-swift | Guest Agent gRPC | >= 1.27.0 |
| opentelemetry-swift | 텔레메트리 | main (branch) |

### 빌드 명령

```bash
# 디버그 빌드
swift build

# 릴리스 빌드
swift build -c release

# 테스트 실행
swift test

# Homebrew 설치
brew install cirruslabs/cli/tart
```

## 3. 모듈 구조

Tart는 **단일 타겟**(실행 파일)으로 구성됩니다. Swift 모듈 분리 대신 **디렉토리 기반** 논리적 모듈화를 사용합니다.

```
┌─────────────────────────────────────────────────┐
│                   tart (Executable)               │
├─────────────────────────────────────────────────┤
│  Commands/    │  CLI 서브커맨드 레이어              │
│  (18+ files)  │  ArgumentParser 기반               │
├───────────────┼─────────────────────────────────┤
│  VM*.swift    │  VM 관리 레이어                    │
│  Config.swift │  설정/스토리지/디렉토리              │
├───────────────┼─────────────────────────────────┤
│  OCI/         │  OCI 레지스트리 프로토콜            │
│  Credentials/ │  인증                             │
├───────────────┼─────────────────────────────────┤
│  Platform/    │  플랫폼 추상화                     │
│  Network/     │  네트워크 추상화                    │
│  VNC/         │  디스플레이                        │
├───────────────┼─────────────────────────────────┤
│  FileLock     │  인프라 레이어                     │
│  PIDLock      │  잠금, 캐시, 유틸리티              │
│  Fetcher      │                                  │
└─────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────┐
│          Apple Virtualization.Framework           │
│  VZVirtualMachine, VZVirtualMachineConfiguration  │
│  VZMacOSBootLoader, VZEFIBootLoader              │
│  VZNATNetworkDeviceAttachment, ...               │
└─────────────────────────────────────────────────┘
```

## 4. 핵심 프로토콜 (의존성 역전)

Tart는 Swift 프로토콜을 활용한 추상화가 곳곳에 적용되어 있습니다.

### Platform 프로토콜 (`Sources/tart/Platform/Platform.swift`)

```swift
protocol Platform: Codable {
  func os() -> OS
  func bootLoader(nvramURL: URL) throws -> VZBootLoader
  func platform(nvramURL: URL, needsNestedVirtualization: Bool) throws -> VZPlatformConfiguration
  func graphicsDevice(vmConfig: VMConfig) -> VZGraphicsDeviceConfiguration
  func keyboards() -> [VZKeyboardConfiguration]
  func pointingDevices() -> [VZPointingDeviceConfiguration]
  func pointingDevicesSimplified() -> [VZPointingDeviceConfiguration]
}
```

- **Darwin** (`Darwin.swift`): `VZMacOSBootLoader`, `VZMacPlatformConfiguration`, `VZMacGraphicsDeviceConfiguration`
- **Linux** (`Linux.swift`): `VZEFIBootLoader`, `VZGenericPlatformConfiguration`, `VZVirtioGraphicsDeviceConfiguration`

### Network 프로토콜 (`Sources/tart/Network/Network.swift`)

```swift
protocol Network {
  func attachments() -> [VZNetworkDeviceAttachment]
  func run(_ sema: AsyncSemaphore) throws
  func stop() async throws
}
```

- **NetworkShared**: `VZNATNetworkDeviceAttachment` (기본)
- **NetworkBridged**: `VZBridgedNetworkDeviceAttachment`
- **Softnet**: `VZFileHandleNetworkDeviceAttachment` + 외부 softnet 프로세스

### CredentialsProvider 프로토콜 (`Sources/tart/Credentials/CredentialsProvider.swift`)

```swift
protocol CredentialsProvider {
  var userFriendlyName: String { get }
  func retrieve(host: String) throws -> (String, String)?
  func store(host: String, user: String, password: String) throws
}
```

순서: `EnvironmentCredentialsProvider` → `DockerConfigCredentialsProvider` → `KeychainCredentialsProvider`

### Prunable 프로토콜

```swift
protocol Prunable {
  var url: URL { get }
  func accessDate() throws -> Date
  func allocatedSizeBytes() throws -> Int
  func delete() throws
}
```

VMDirectory가 Prunable을 준수하여 자동 프루닝 대상이 됩니다.

## 5. Swift 동시성 패턴

### async/await

Tart는 Swift Concurrency를 전면 활용합니다:

```swift
// Root.swift - 비동기 진입점
@main
struct Root: AsyncParsableCommand {
  public static func main() async throws { ... }
}

// 서브커맨드도 AsyncParsableCommand
struct Run: AsyncParsableCommand {
  func run() async throws { ... }
}
```

### withTaskCancellationHandler

VM 생성/클론/Pull 등에서 취소 처리:

```swift
// Create.swift:48
try await withTaskCancellationHandler(operation: {
  // VM 생성 로직
  _ = try await VM(vmDir: tmpVMDir, ipswURL: ipswURL, ...)
  try VMStorageLocal().move(name, from: tmpVMDir)
}, onCancel: {
  try? FileManager.default.removeItem(at: tmpVMDir.baseURL)
})
```

### AsyncSemaphore

VM 실행 대기를 위한 비동기 세마포어:

```swift
// VM.swift:33
var sema = AsyncSemaphore(value: 0)

// VM.swift:270 - 게스트가 종료될 때까지 대기
func run() async throws {
  try await sema.waitUnlessCancelled()
}

// VM.swift:447 - delegate에서 시그널
func guestDidStop(_ virtualMachine: VZVirtualMachine) {
  sema.signal()
}
```

### withThrowingTaskGroup

OCI Push/Pull에서 병렬 처리:

```swift
// DiskV2.swift:33
try await withThrowingTaskGroup(of: (Int, OCIManifestLayer).self) { group in
  for (index, data) in mappedDisk.chunks(ofCount: layerLimitBytes).enumerated() {
    if index >= concurrency {
      if let (index, pushedLayer) = try await group.next() {
        pushedLayers.append((index, pushedLayer))
      }
    }
    group.addTask { ... }
  }
}
```

## 6. 파일 시스템 레이아웃 (~/.tart/)

```
~/.tart/                             # TART_HOME (환경변수로 변경 가능)
├── vms/                             # 로컬 VM 저장소 (VMStorageLocal)
│   ├── my-vm/                       # VMDirectory
│   │   ├── config.json              # VMConfig (JSON)
│   │   ├── disk.img                 # 디스크 이미지 (RAW 또는 ASIF)
│   │   ├── nvram.bin                # NVRAM (부트 설정)
│   │   ├── state.vzvmsave           # 일시정지 상태 (Suspend)
│   │   └── control.sock             # 제어 Unix 소켓
│   └── another-vm/
│       └── ...
├── cache/                           # 캐시
│   └── OCIs/                        # OCI 이미지 캐시 (VMStorageOCI)
│       └── ghcr.io/
│           └── cirruslabs/
│               └── macos-tahoe-base/
│                   ├── latest -> sha256:abc...  # 태그 (심볼릭 링크)
│                   └── sha256:abc.../           # 다이제스트 (실제 데이터)
│                       ├── config.json
│                       ├── disk.img
│                       ├── nvram.bin
│                       └── manifest.json
└── tmp/                             # 임시 디렉토리 (GC 대상)
    └── <uuid>/                      # 작업 중 임시 VM
```

## 7. 테스트 구조

```
Tests/TartTests/
├── VMConfigTests.swift              # VMConfig 직렬화/역직렬화
├── RegistryTests.swift              # OCI 레지스트리 프로토콜
├── ReferenceTests.swift             # OCI 참조 파싱
└── ...
```

통합 테스트는 `integration-tests/tart/` 에 별도로 위치합니다.

## 8. 커맨드 구조 요약

| 커맨드 | 소스 파일 | 핵심 동작 |
|--------|----------|----------|
| `create` | Create.swift | IPSW/Linux VM 생성, 디스크 할당 |
| `clone` | Clone.swift | 로컬/리모트 VM 복제, APFS CoW |
| `run` | Run.swift | VM 실행, GUI 윈도우, VNC |
| `set` | Set.swift | CPU/메모리/디스크/디스플레이 변경 |
| `get` | Get.swift | VM 설정 조회 |
| `list` | List.swift | 로컬+OCI VM 목록 |
| `pull` | Pull.swift | OCI 레지스트리에서 Pull |
| `push` | Push.swift | OCI 레지스트리로 Push |
| `exec` | Exec.swift | Guest Agent로 VM 내부 명령 |
| `ip` | IP.swift | VM IP 주소 조회 |
| `stop` | Stop.swift | 실행 중 VM 중지 |
| `suspend` | Suspend.swift | VM 일시정지 (macOS 14+) |
| `delete` | Delete.swift | VM 삭제 |
| `import` | Import.swift | Apple Archive에서 가져오기 |
| `export` | Export.swift | Apple Archive로 내보내기 |
| `login` | Login.swift | 레지스트리 로그인 |
| `logout` | Logout.swift | 레지스트리 로그아웃 |
| `rename` | Rename.swift | VM 이름 변경 |
| `prune` | Prune.swift | 캐시/VM 정리 |
| `fqn` | FQN.swift | 정규화된 이름 출력 |
