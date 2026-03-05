# 05. Tart 핵심 컴포넌트

## 1. VM 클래스 — 가상 머신의 심장

### 개요

`VM` 클래스(`Sources/tart/VM.swift`)는 Apple `Virtualization.Framework`의 `VZVirtualMachine`을 감싸는 핵심 래퍼입니다.

```
VM (NSObject, VZVirtualMachineDelegate, ObservableObject)
├── virtualMachine: VZVirtualMachine       # 실제 가상 머신 인스턴스
├── configuration: VZVirtualMachineConfiguration  # VZ 구성
├── sema: AsyncSemaphore                   # 비동기 라이프사이클 동기화
├── name: String                           # VM 이름
├── config: VMConfig                       # Tart 자체 설정
└── network: Network                       # 네트워크 추상화
```

### 초기화 (기존 VM 열기)

`VM.init(vmDir:network:additionalStorageDevices:...)`는 기존 VM을 여는 경로입니다:

```swift
// VM.swift:43
init(vmDir: VMDirectory, network: Network = NetworkShared(), ...) throws {
    name = vmDir.name
    config = try VMConfig.init(fromURL: vmDir.configURL)  // config.json 로드

    if config.arch != CurrentArchitecture() {
        throw UnsupportedArchitectureError()  // 아키텍처 불일치 체크
    }

    self.network = network
    configuration = try Self.craftConfiguration(...)  // VZ 구성 생성
    virtualMachine = VZVirtualMachine(configuration: configuration)

    super.init()
    virtualMachine.delegate = self  // delegate 등록
}
```

### 초기화 (macOS VM 새로 생성)

`VM.init(vmDir:ipswURL:diskSizeGB:...)` — arm64 전용, IPSW 기반 macOS 설치:

```swift
// VM.swift:146 (#if arch(arm64))
init(vmDir: VMDirectory, ipswURL: URL, diskSizeGB: UInt16, ...) async throws {
    // 1. IPSW URL이 리모트면 다운로드
    if !ipswURL.isFileURL {
        ipswURL = try await VM.retrieveIPSW(remoteURL: ipswURL)
    }

    // 2. 복원 이미지 로드
    let image = try await VZMacOSRestoreImage.load(from: ipswURL)
    let requirements = image.mostFeaturefulSupportedConfiguration!

    // 3. NVRAM 생성
    _ = try VZMacAuxiliaryStorage(creatingStorageAt: vmDir.nvramURL,
                                   hardwareModel: requirements.hardwareModel)

    // 4. 디스크 생성
    try vmDir.resizeDisk(diskSizeGB, format: diskFormat)

    // 5. VMConfig 생성 및 저장
    config = VMConfig(platform: Darwin(ecid: VZMacMachineIdentifier(),
                                        hardwareModel: requirements.hardwareModel), ...)
    try config.setCPU(cpuCount: max(4, requirements.minimumSupportedCPUCount))
    try config.save(toURL: vmDir.configURL)

    // 6. VZ 구성 및 VM 생성
    configuration = try Self.craftConfiguration(...)
    virtualMachine = VZVirtualMachine(configuration: configuration)

    // 7. macOS 설치 실행
    try await install(ipswURL)
}
```

### craftConfiguration — VZ 구성 조립

`VM.craftConfiguration()`은 VZ 프레임워크의 모든 가상 디바이스를 조립합니다:

```
VZVirtualMachineConfiguration
├── bootLoader       ← platform.bootLoader() (Mac/EFI)
├── cpuCount         ← vmConfig.cpuCount
├── memorySize       ← vmConfig.memorySize
├── platform         ← platform.platform() (Mac/Generic)
├── graphicsDevices  ← platform.graphicsDevice()
├── audioDevices     ← VZVirtioSoundDevice (input + output)
├── keyboards        ← platform.keyboards() (USB + Mac)
├── pointingDevices  ← platform.pointingDevices() (USB + Trackpad)
├── networkDevices   ← network.attachments() + vmConfig.macAddress
├── consoleDevices   ← Spice clipboard + version console
├── storageDevices   ← VZDiskImageStorageDeviceAttachment + additional
├── entropyDevices   ← VZVirtioEntropyDevice (비 suspendable 시)
├── directorySharingDevices ← 호스트 디렉토리 공유
├── serialPorts      ← 시리얼 포트
└── socketDevices    ← VZVirtioSocketDevice (Guest Agent용)
```

### VM 라이프사이클

```
                  ┌──── start(recovery) ────┐
                  │                         ▼
 [Stopped] ──── start() ──── [Running] ──── stop() ──── [Stopped]
    │                            │
    │                            ├── sema.wait (vm.run())
    │                            │
    └── resume() ────────────── [Running]
         (from Suspended)        │
                                 ├── guestDidStop() → sema.signal()
                                 └── didStopWithError() → sema.signal()
```

```swift
// VM.swift:247
func start(recovery: Bool, resume shouldResume: Bool) async throws {
    try network.run(sema)        // 네트워크 시작 (Softnet의 경우 프로세스 실행)

    if shouldResume {
        try await resume()       // 일시정지 상태에서 복원
    } else {
        try await start(recovery) // 새로 시작
    }
}

// VM.swift:270
func run() async throws {
    do {
        try await sema.waitUnlessCancelled()  // 게스트 종료 또는 취소 대기
    } catch is CancellationError {
        // Ctrl+C 또는 tart stop에 의한 취소
    }

    if Task.isCancelled {
        if virtualMachine.state == .running {
            print("Stopping VM...")
            try await stop()
        }
    }

    try await network.stop()
}
```

## 2. VMConfig — JSON 직렬화 가능한 VM 설정

### 구조

```swift
// VMConfig.swift:56
struct VMConfig: Codable {
    var version: Int = 1                    // 설정 스키마 버전
    var os: OS                              // .darwin 또는 .linux
    var arch: Architecture                  // .arm64
    var platform: Platform                  // Darwin 또는 Linux
    var cpuCountMin: Int                    // 최소 CPU 수
    private(set) var cpuCount: Int          // 현재 CPU 수
    var memorySizeMin: UInt64               // 최소 메모리 (바이트)
    private(set) var memorySize: UInt64     // 현재 메모리 (바이트)
    var macAddress: VZMACAddress            // MAC 주소 (랜덤 생성)
    var display: VMDisplayConfig = VMDisplayConfig()  // 디스플레이 (1024x768)
    var displayRefit: Bool?                 // 디스플레이 리핏
    var diskFormat: DiskImageFormat = .raw  // RAW 또는 ASIF
}
```

### 유효성 검증

```swift
// VMConfig.swift:164
mutating func setCPU(cpuCount: Int) throws {
    // macOS VM: 운영체제 최소 요구사항 체크
    if os == .darwin && cpuCount < cpuCountMin { throw ... }
    // VZ 프레임워크 최소 요구사항 체크
    if cpuCount < VZVirtualMachineConfiguration.minimumAllowedCPUCount { throw ... }
    self.cpuCount = cpuCount
}
```

### JSON 직렬화

`config.json` 예시:
```json
{
    "version": 1,
    "os": "darwin",
    "arch": "arm64",
    "ecid": "base64...",
    "hardwareModel": "base64...",
    "cpuCountMin": 4,
    "cpuCount": 8,
    "memorySizeMin": 4294967296,
    "memorySize": 8589934592,
    "macAddress": "7a:9c:45:12:ab:cd",
    "display": {"width": 1920, "height": 1080},
    "diskFormat": "raw"
}
```

## 3. VMDirectory — VM 파일 번들

### 구조

```swift
// VMDirectory.swift:5
struct VMDirectory: Prunable {
    var baseURL: URL                     // VM 루트 디렉토리

    var configURL: URL  { baseURL.appendingPathComponent("config.json") }
    var diskURL: URL    { baseURL.appendingPathComponent("disk.img") }
    var nvramURL: URL   { baseURL.appendingPathComponent("nvram.bin") }
    var stateURL: URL   { baseURL.appendingPathComponent("state.vzvmsave") }
    var manifestURL: URL { baseURL.appendingPathComponent("manifest.json") }
    var controlSocketURL: URL { URL(fileURLWithPath: "control.sock", relativeTo: baseURL) }
}
```

### 상태 판별

```swift
// VMDirectory.swift:6
enum State: String {
    case Running = "running"     // PID 잠금 활성
    case Suspended = "suspended" // state.vzvmsave 파일 존재
    case Stopped = "stopped"     // 기본 상태
}

// VMDirectory.swift:62
func state() throws -> State {
    if try running() { return .Running }           // PIDLock으로 확인
    else if FileManager.default.fileExists(atPath: stateURL.path) { return .Suspended }
    else { return .Stopped }
}
```

### 디스크 리사이즈

```swift
// VMDirectory.swift:145
func resizeDisk(_ sizeGB: UInt16, format: DiskImageFormat) throws {
    let diskExists = FileManager.default.fileExists(atPath: diskURL.path)
    if diskExists {
        try resizeExistingDisk(sizeGB)  // RAW: truncate, ASIF: diskutil
    } else {
        try createDisk(sizeGB: sizeGB, format: format)  // 새로 생성
    }
}
```

RAW 디스크는 `FileHandle.truncate(atOffset:)`로 간단히 크기 변경, ASIF는 `diskutil image resize` 프로세스 호출.

### 클론 (APFS Copy-on-Write)

```swift
// VMDirectory.swift:119
func clone(to: VMDirectory, generateMAC: Bool) throws {
    try FileManager.default.copyItem(at: configURL, to: to.configURL)
    try FileManager.default.copyItem(at: nvramURL, to: to.nvramURL)
    try FileManager.default.copyItem(at: diskURL, to: to.diskURL)   // APFS CoW!
    try? FileManager.default.copyItem(at: stateURL, to: to.stateURL)

    if generateMAC {
        try to.regenerateMACAddress()  // MAC 충돌 방지
    }
}
```

APFS에서 `copyItem`은 실제로 블록을 복사하지 않고 참조만 추가합니다. 변경이 발생한 블록만 새로 할당됩니다.

## 4. Registry — OCI 레지스트리 클라이언트

### 구조

```swift
// Registry.swift:113
class Registry {
    private let baseURL: URL              // https://ghcr.io/v2/
    let namespace: String                 // cirruslabs/macos-tahoe-base
    let credentialsProviders: [CredentialsProvider]
    let authenticationKeeper = AuthenticationKeeper()
}
```

### 핵심 메서드

| 메서드 | HTTP | 용도 |
|--------|------|------|
| `pullManifest(reference:)` | GET /manifests/{ref} | OCI 매니페스트 조회 |
| `pushManifest(reference:manifest:)` | PUT /manifests/{ref} | 매니페스트 업로드 |
| `pullBlob(digest:handler:)` | GET /blobs/{digest} | Blob 스트리밍 다운로드 |
| `pushBlob(fromData:chunkSize:)` | POST+PUT /blobs/uploads/ | Blob 업로드 (모놀리식/청크) |
| `blobExists(digest:)` | HEAD /blobs/{digest} | Blob 존재 확인 |

### 인증 흐름

```
요청 → 401 Unauthorized
  → WWW-Authenticate 헤더 파싱 (Bearer/Basic)
  → Bearer: realm URL에서 토큰 요청 (scope, service 파라미터)
  → Basic: Keychain/Docker config/환경변수에서 자격증명 조회
  → 원래 요청 재시도 (Authorization 헤더 추가)
```

인증 프로바이더 체인:
1. `EnvironmentCredentialsProvider` — `TART_REGISTRY_USERNAME` / `TART_REGISTRY_PASSWORD`
2. `DockerConfigCredentialsProvider` — `~/.docker/config.json` (auths, credHelpers)
3. `KeychainCredentialsProvider` — macOS Keychain

## 5. Network — 네트워크 추상화

### 세 가지 네트워크 모드

```
┌──────────────────────────────────────────────┐
│               Network 프로토콜                 │
│  attachments() → [VZNetworkDeviceAttachment]  │
│  run(sema) throws                             │
│  stop() async throws                          │
└──────┬──────────────┬──────────────┬─────────┘
       │              │              │
  NetworkShared   NetworkBridged   Softnet
  (NAT, 기본)    (브리지)         (소켓페어)
```

| 모드 | 클래스 | VZ Attachment | 특징 |
|------|--------|---------------|------|
| Shared | `NetworkShared` | `VZNATNetworkDeviceAttachment` | 기본, NAT, 추가 설정 불필요 |
| Bridged | `NetworkBridged` | `VZBridgedNetworkDeviceAttachment` | 물리 인터페이스 직접 연결 |
| Softnet | `Softnet` | `VZFileHandleNetworkDeviceAttachment` | Unix 소켓 페어 + 외부 프로세스 |

### Softnet 상세

```swift
// Softnet.swift:19
init(vmMACAddress: String, extraArguments: [String]) throws {
    // Unix 소켓 페어 생성
    let fds = UnsafeMutablePointer<Int32>.allocate(capacity: ...)
    socketpair(AF_UNIX, SOCK_DGRAM, 0, fds)

    vmFD = fds[0]           // VM에 연결할 FD
    let softnetFD = fds[1]  // softnet 프로세스에 전달할 FD

    // 버퍼 크기 설정 (SO_RCVBUF = 4*1MB, SO_SNDBUF = 1MB)
    try setSocketBuffers(vmFD, 1 * 1024 * 1024)
    try setSocketBuffers(softnetFD, 1 * 1024 * 1024)

    // 외부 softnet 프로세스 설정
    process.executableURL = try Self.softnetExecutableURL()
    process.arguments = ["--vm-fd", String(STDIN_FILENO), "--vm-mac-address", vmMACAddress]
    process.standardInput = FileHandle(fileDescriptor: softnetFD)
}
```

## 6. DiskV2 — 디스크 레이어 관리

### Push (압축 + 업로드)

```
disk.img (수 GB)
    │
    ├── chunk[0] (512MB) ── LZ4 압축 ── pushBlob() ── layer[0]
    ├── chunk[1] (512MB) ── LZ4 압축 ── pushBlob() ── layer[1]
    ├── chunk[2] (512MB) ── LZ4 압축 ── pushBlob() ── layer[2]
    └── chunk[n] (나머지) ── LZ4 압축 ── pushBlob() ── layer[n]

    ※ 병렬 실행 (concurrency 파라미터로 제어)
```

### Pull (다운로드 + 해제)

```
layer[0..n] (레지스트리)
    │
    ├── pullBlob() ── LZ4 해제 ── zeroSkippingWrite() ── disk[offset]
    ├── LocalLayerCache 활용 (이전 버전 디스크에서 재사용)
    └── 재개 가능 Pull (rangeStart, 기존 레이어 해시 검증)

    zeroSkippingWrite: 0으로 채워진 청크는 건너뜀 → 희소 파일 최적화
```

### 제로 스킵 쓰기 최적화

```swift
// DiskV2.swift:247
private static func zeroSkippingWrite(...) throws -> UInt64 {
    for chunk in data.chunks(ofCount: holeGranularityBytes) {
        if chunk == zeroChunk {   // 4MB 단위 제로 체크
            // 쓰기 건너뜀 (truncate로 이미 0)
            // 또는 F_PUNCHHOLE로 홀 생성
        } else {
            try disk.seek(toOffset: offset)
            try disk.write(contentsOf: chunk)
        }
    }
}
```

## 7. Config — 전역 설정

```swift
// Config.swift:3
struct Config {
    let tartHomeDir: URL   // ~/.tart 또는 $TART_HOME
    let tartCacheDir: URL  // ~/.tart/cache
    let tartTmpDir: URL    // ~/.tart/tmp

    init() throws {
        // TART_HOME 환경변수 우선
        if let customTartHome = ProcessInfo.processInfo.environment["TART_HOME"] {
            tartHomeDir = URL(fileURLWithPath: customTartHome)
        } else {
            tartHomeDir = FileManager.default
                .homeDirectoryForCurrentUser
                .appendingPathComponent(".tart")
        }
        // cache, tmp 디렉토리 자동 생성
    }

    func gc() throws {
        // tmp 디렉토리 내 잠금 해제된 항목 제거
        for entry in try FileManager.default.contentsOfDirectory(at: tartTmpDir, ...) {
            let lock = try FileLock(lockURL: entry)
            if try !lock.trylock() { continue }  // 사용 중이면 건너뜀
            try FileManager.default.removeItem(at: entry)
            try lock.unlock()
        }
    }
}
```

## 8. Fetcher — 비동기 HTTP 클라이언트

```swift
// Fetcher.swift:19
class Fetcher {
    static func fetch(_ request: URLRequest, viaFile: Bool) async throws
        -> (AsyncThrowingStream<Data, Error>, HTTPURLResponse) {

        let task = urlSession.dataTask(with: request)
        let delegate = Delegate()  // URLSessionDataDelegate
        task.delegate = delegate

        // AsyncThrowingStream으로 데이터 스트리밍
        let stream = AsyncThrowingStream<Data, Error> { continuation in
            delegate.streamContinuation = continuation
        }

        // 응답 대기
        let response = try await withCheckedThrowingContinuation { continuation in
            delegate.responseContinuation = continuation
            task.resume()
        }

        return (stream, response as! HTTPURLResponse)
    }
}
```

16MB 버퍼를 사용하여 데이터를 청크 단위로 스트리밍합니다. 쿠키 자동 전송은 비활성화(`httpShouldSetCookies = false`)하여 Harbor CSRF 문제를 회피합니다.

## 9. 잠금 메커니즘

### FileLock (flock 기반)

```swift
// FileLock.swift:9
class FileLock {
    let fd: Int32
    func trylock() throws -> Bool { flockWrapper(LOCK_EX | LOCK_NB) }
    func lock() throws           { _ = flockWrapper(LOCK_EX) }
    func unlock() throws         { _ = flockWrapper(LOCK_UN) }
}
```

용도: 임시 디렉토리 GC 보호, OCI Pull 동시성 제어, VM Push 중 보호

### PIDLock (fcntl 기반)

```swift
// PIDLock.swift:4
class PIDLock {
    let fd: Int32
    func trylock() throws -> Bool  // F_SETLK + F_WRLCK
    func lock() throws             // F_SETLKW + F_WRLCK (대기)
    func unlock() throws           // F_SETLK + F_UNLCK
    func pid() throws -> pid_t     // F_GETLK으로 잠금 소유자 PID 조회
}
```

용도: VM 실행 상태 확인 — `VMDirectory.running()`은 `lock.pid() != 0`으로 판단

## 10. Root — 진입점

```swift
// Root.swift:9
@main
struct Root: AsyncParsableCommand {
    static var configuration = CommandConfiguration(
        commandName: "tart",
        version: CI.version,
        subcommands: [Create.self, Clone.self, Run.self, Set.self, Get.self,
                      List.self, Login.self, Logout.self, IP.self, Exec.self,
                      Pull.self, Push.self, Import.self, Export.self, Prune.self,
                      Rename.self, Stop.self, Delete.self, FQN.self]
    )

    public static func main() async throws {
        // macOS 14+에서 Suspend 커맨드 동적 추가
        if #available(macOS 14, *) {
            configuration.subcommands.append(Suspend.self)
        }

        signal(SIGINT, SIG_IGN)  // 기본 SIGINT 핸들러 비활성화
        // Ctrl+C → Task.cancel()로 매핑
        let sigintSrc = DispatchSource.makeSignalSource(signal: SIGINT)
        sigintSrc.setEventHandler { task.cancel() }
        sigintSrc.activate()

        defer { OTel.shared.flush() }  // OpenTelemetry 플러시

        var command = try parseAsRoot()

        // 루트 스팬 생성 (OpenTelemetry)
        let span = OTel.shared.tracer.spanBuilder(spanName: ...).startSpan()

        // Pull/Clone 외 커맨드에서 GC 실행
        if type(of: command) != type(of: Pull()) && type(of: command) != type(of: Clone()) {
            try Config().gc()
        }

        // 커맨드 실행
        if var asyncCommand = command as? AsyncParsableCommand {
            try await asyncCommand.run()
        } else {
            try command.run()
        }
    }
}
```
