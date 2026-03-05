# 13. CLI 커맨드 시스템 심화

## 1. 개요

Tart의 CLI는 Apple의 `swift-argument-parser` 라이브러리를 기반으로 구축된 비동기
커맨드 시스템이다. `Root` 구조체가 `@main` 진입점이며, 19개의 서브커맨드(macOS 14
이상에서는 `Suspend` 포함하여 20개)를 제공한다. 각 커맨드는 `AsyncParsableCommand`
프로토콜을 준수하여 `async/await` 패턴을 자연스럽게 사용한다.

### 해결해야 할 핵심 과제

1. **다양한 VM 작업의 통합 인터페이스**: 생성, 실행, 복제, Push/Pull 등 20개 이상의
   작업을 일관된 CLI로 제공해야 한다
2. **비동기 작업 지원**: 네트워크 I/O(Pull, Push), VM 실행 등 대부분의 작업이 비동기다
3. **안전한 취소 처리**: Ctrl+C 시 임시 파일 정리, VM 정상 종료 등을 보장해야 한다
4. **macOS 버전별 호환성**: Virtualization.Framework API가 macOS 버전마다 다르다
5. **CI/CD 자동화 지원**: 종료 코드, JSON 출력 등 스크립트 친화적이어야 한다

```
┌──────────────────────────────────────────────────────┐
│              Root (@main AsyncParsableCommand)        │
│  CommandConfiguration: "tart"                         │
│  서브커맨드: Create, Clone, Run, Set, Get, List, ...   │
└──────────────────────────────────────────────────────┘
        │
        │  main() async throws
        │
        1. signal(SIGINT, SIG_IGN) + DispatchSource → Task.cancel()
        2. setlinebuf(stdout)
        3. parseAsRoot() → 커맨드 파싱
        4. OTel 스팬 생성
        5. Config().gc() → 임시 디렉토리 정리 (Pull/Clone 제외)
        6. command.run() → 커맨드 실행
        7. OTel.shared.flush() → 텔레메트리 전송
```

## 2. swift-argument-parser 의존성

소스 파일: `Package.swift`

```swift
// Package.swift
dependencies: [
  .package(url: "https://github.com/apple/swift-argument-parser", from: "1.6.1"),
  // ...
],
targets: [
  .executableTarget(name: "tart", dependencies: [
    .product(name: "ArgumentParser", package: "swift-argument-parser"),
    // ...
  ]),
]
```

**왜 swift-argument-parser인가?** Apple 공식 라이브러리로, Swift의 타입 시스템과
프로퍼티 래퍼를 활용하여 커맨드라인 인자를 선언적으로 정의한다. `@Argument`, `@Option`,
`@Flag` 프로퍼티 래퍼로 인자를 정의하면 자동으로 파싱, 유효성 검증, 도움말 생성이 이루어진다.
또한 `AsyncParsableCommand`를 지원하여 async/await와 자연스럽게 통합된다.

## 3. Root.swift - 진입점 분석

소스 파일: `Sources/tart/Root.swift`

### 구조체 정의와 서브커맨드 등록

```swift
@main
struct Root: AsyncParsableCommand {
  static var configuration = CommandConfiguration(
    commandName: "tart",
    version: CI.version,
    subcommands: [
      Create.self,
      Clone.self,
      Run.self,
      Set.self,
      Get.self,
      List.self,
      Login.self,
      Logout.self,
      IP.self,
      Exec.self,
      Pull.self,
      Push.self,
      Import.self,
      Export.self,
      Prune.self,
      Rename.self,
      Stop.self,
      Delete.self,
      FQN.self,
    ])
```

19개의 서브커맨드가 정적으로 등록되며, `Suspend` 커맨드는 런타임에 macOS 버전을
확인하여 조건부로 추가된다:

```swift
public static func main() async throws {
  // macOS 14+ 에서만 Suspend 커맨드 추가
  if #available(macOS 14, *) {
    configuration.subcommands.append(Suspend.self)
  }
```

**왜 조건부 추가인가?** `Suspend` 커맨드는 `VZVirtualMachine.saveMachineStateTo(url:)`를
사용하는데, 이 API는 macOS 14(Sonoma) 이상에서만 사용 가능하다. macOS 13에서 해당
커맨드를 노출하면 사용자가 실행 시 런타임 에러를 겪으므로, 아예 커맨드 목록에서 제거한다.

### SIGINT 처리 - 구조적 동시성과 시그널의 통합

```swift
// 기본 SIGINT 핸들러 비활성화
signal(SIGINT, SIG_IGN);

// 커스텀 핸들러: Task.cancel() 호출
let task = withUnsafeCurrentTask { $0 }!
let sigintSrc = DispatchSource.makeSignalSource(signal: SIGINT)
sigintSrc.setEventHandler {
  task.cancel()
}
sigintSrc.activate()
```

이 패턴의 핵심은 **Swift Structured Concurrency와 Unix 시그널 처리의 통합**이다:

1. `signal(SIGINT, SIG_IGN)`: 기본 SIGINT 핸들러(프로세스 즉시 종료)를 비활성화
2. `DispatchSource.makeSignalSource(signal: SIGINT)`: GCD 기반 시그널 모니터링
3. `task.cancel()`: 현재 실행 중인 Task의 취소 플래그 설정

**왜 두 핸들러가 경쟁하면 안 되는가?** 기본 SIGINT 핸들러와 GCD 시그널 소스가 동시에
활성화되면, SIGINT 수신 시 어떤 핸들러가 먼저 실행될지 보장할 수 없다. 기본 핸들러가
먼저 실행되면 프로세스가 즉시 종료되어 정리 작업(VM 중지, 임시 파일 삭제 등)이 수행되지
않는다.

```
SIGINT 처리 흐름:

  Ctrl+C 또는 kill -INT
    │
    ├── 기본 핸들러 (SIG_IGN으로 비활성화)
    │     → 무시됨
    │
    └── DispatchSource 핸들러
          → task.cancel()
            → Task.isCancelled == true
              → withTaskCancellationHandler의 onCancel 실행
                → 임시 디렉토리 삭제 등 정리 작업
```

### 라인 버퍼링

```swift
// Set line-buffered output for stdout
setlinebuf(stdout)
```

**왜 라인 버퍼링인가?** 기본적으로 stdout이 파이프에 연결되면(예: `tart pull ... | tee log.txt`)
블록 버퍼링이 적용되어 출력이 지연된다. `setlinebuf()`로 라인 버퍼링을 설정하면
각 줄이 즉시 출력되어 진행 상황을 실시간으로 확인할 수 있다.

### 커맨드 라이프사이클

```swift
defer { OTel.shared.flush() }

do {
  // 1. 커맨드 파싱
  var command = try parseAsRoot()

  // 2. OTel 루트 스팬 생성
  let span = OTel.shared.tracer.spanBuilder(
    spanName: type(of: command)._commandName).startSpan()
  defer { span.end() }
  OpenTelemetry.instance.contextProvider.setActiveSpan(span)

  // 3. 커맨드라인 인자를 스팬 속성에 기록
  let commandLineArguments = ProcessInfo.processInfo.arguments.map { argument in
    AttributeValue.string(argument)
  }
  span.setAttribute(key: "Command-line arguments",
                     value: .array(AttributeArray(values: commandLineArguments)))

  // 4. Cirrus CI 태그 기록
  if let tags = ProcessInfo.processInfo.environment["CIRRUS_SENTRY_TAGS"] {
    for (key, value) in tags.split(separator: ",").compactMap(splitEnvironmentVariable) {
      span.setAttribute(key: key, value: .string(value))
    }
  }

  // 5. GC 실행 (Pull, Clone 제외)
  if type(of: command) != type(of: Pull()) && type(of: command) != type(of: Clone()){
    do {
      try Config().gc()
    } catch {
      fputs("Failed to perform garbage collection: \(error)\n", stderr)
    }
  }

  // 6. 커맨드 실행
  if var asyncCommand = command as? AsyncParsableCommand {
    try await asyncCommand.run()
  } else {
    try command.run()
  }
```

**왜 Pull과 Clone에서 GC를 건너뛰는가?** Pull과 Clone은 자체적으로 자동
프루닝(`Prune.reclaimIfNeeded()`)을 수행한다. GC가 임시 디렉토리를 먼저 정리하면,
Pull/Clone의 이전 시도에서 남긴 부분 다운로드 데이터가 손실될 수 있다. 또한 Pull과
Clone이 직접 임시 디렉토리에 잠금을 설정하므로 GC와의 레이스 컨디션을 방지한다.

**왜 GC 실패를 무시하는가?** GC는 "최선의 노력(best-effort)" 작업이다. 임시 디렉토리
정리에 실패해도 핵심 커맨드 실행에는 영향이 없다. 에러는 stderr에 출력하되 프로세스를
종료하지 않는다.

### 에러 처리 체계

```swift
} catch {
  // tart exec의 원격 명령 종료 코드 전달
  if let execCustomExitCodeError = error as? ExecCustomExitCodeError {
    OTel.shared.flush()
    Foundation.exit(execCustomExitCodeError.exitCode)
  }

  // OpenTelemetry에 에러 기록
  OpenTelemetry.instance.contextProvider.activeSpan?.recordException(error)

  // 커스텀 종료 코드가 있는 에러
  if let errorWithExitCode = error as? HasExitCode {
    fputs("\(error)\n", stderr)
    OTel.shared.flush()
    Foundation.exit(errorWithExitCode.exitCode)
  }

  // ArgumentParser 에러 (도움말, 유효성 검증 실패 등)
  exit(withError: error)
}
```

```
에러 처리 우선순위:

  에러 발생
    │
    ├── 1. ExecCustomExitCodeError?
    │     → OTel flush + Foundation.exit(customCode)
    │     (tart exec의 원격 명령 종료 코드를 그대로 전달)
    │
    ├── 2. HasExitCode 프로토콜 준수?
    │     → stderr 출력 + OTel flush + Foundation.exit(exitCode)
    │     (RuntimeError의 종료 코드 매핑)
    │
    └── 3. 그 외 (ArgumentParser 에러 등)
          → exit(withError:) → 도움말 또는 에러 메시지 출력
```

### RuntimeError 종료 코드 매핑

```swift
// Sources/tart/VMStorageHelper.swift
extension RuntimeError : HasExitCode {
  var exitCode: Int32 {
    switch self {
    case .VMDoesNotExist: return 2
    case .VMNotRunning:   return 2
    case .VMAlreadyRunning: return 2
    default:              return 1
    }
  }
}
```

| 종료 코드 | 에러 유형 | 용도 |
|-----------|---------|------|
| 0 | 성공 | 정상 종료 |
| 1 | 일반 에러 | 대부분의 RuntimeError |
| 2 | VM 상태 에러 | VMDoesNotExist, VMNotRunning, VMAlreadyRunning |
| N | Exec 원격 종료 코드 | `tart exec`에서 원격 명령의 종료 코드 전달 |

**왜 VM 상태 에러를 종료 코드 2로 분리하는가?** CI/CD 스크립트에서 "VM이 존재하지 않음"과
"VM 실행 중 에러 발생"을 구분할 수 있다. 종료 코드 2이면 VM 상태 문제이므로 재시도 없이
건너뛸 수 있고, 종료 코드 1이면 일시적 오류일 수 있으므로 재시도할 수 있다.

## 4. 커맨드 분류

### VM 라이프사이클

```
  Create ──► Run ──► Stop / Suspend ──► Delete
                │
                └──► Exec (실행 중 원격 명령)
```

| 커맨드 | 파일 | 설명 |
|--------|------|------|
| `Create` | `Commands/Create.swift` | macOS/Linux VM 생성 |
| `Run` | `Commands/Run.swift` | VM 실행 (GUI/headless) |
| `Stop` | `Commands/Stop.swift` | VM 중지 (graceful/forceful) |
| `Suspend` | `Commands/Suspend.swift` | VM 일시중지 (macOS 14+) |
| `Delete` | `Commands/Delete.swift` | VM 삭제 |
| `Exec` | `Commands/Exec.swift` | 실행 중 VM에 명령 실행 |

### VM 관리

| 커맨드 | 파일 | 설명 |
|--------|------|------|
| `Clone` | `Commands/Clone.swift` | VM 복제 (로컬/리모트 → 로컬) |
| `Rename` | `Commands/Rename.swift` | 로컬 VM 이름 변경 |
| `Set` | `Commands/Set.swift` | VM 설정 변경 (CPU, 메모리 등) |
| `Get` | `Commands/Get.swift` | VM 설정 조회 |
| `List` | `Commands/List.swift` | VM 목록 조회 |
| `IP` | `Commands/IP.swift` | VM IP 주소 조회 |

### 레지스트리 연동

| 커맨드 | 파일 | 설명 |
|--------|------|------|
| `Pull` | `Commands/Pull.swift` | OCI 레지스트리에서 VM 가져오기 |
| `Push` | `Commands/Push.swift` | VM을 OCI 레지스트리에 업로드 |
| `Login` | `Commands/Login.swift` | 레지스트리 인증 |
| `Logout` | `Commands/Logout.swift` | 레지스트리 인증 해제 |
| `FQN` | `Commands/FQN.swift` | 정규화된 이름 조회 (태그→다이제스트) |

### 데이터 이동 / 유지보수

| 커맨드 | 파일 | 설명 |
|--------|------|------|
| `Import` | `Commands/Import.swift` | `.tvm` 파일에서 VM 가져오기 |
| `Export` | `Commands/Export.swift` | VM을 `.tvm` 파일로 내보내기 |
| `Prune` | `Commands/Prune.swift` | 캐시/VM 정리 |

## 5. 주요 커맨드 상세 분석

### 5.1 Create - VM 생성

소스 파일: `Sources/tart/Commands/Create.swift`

```swift
struct Create: AsyncParsableCommand {
  static var configuration = CommandConfiguration(abstract: "Create a VM")

  @Argument(help: "VM name")
  var name: String

  @Option(help: ArgumentHelp("create a macOS VM using path to the IPSW file or URL "
    + "(or \"latest\", to fetch the latest supported IPSW automatically)", valueName: "path"))
  var fromIPSW: String?

  @Flag(help: "create a Linux VM")
  var linux: Bool = false

  @Option(help: ArgumentHelp("Disk size in GB"))
  var diskSize: UInt16 = 50

  @Option(help: ArgumentHelp("Disk image format"))
  var diskFormat: DiskImageFormat = .raw
```

유효성 검증:

```swift
func validate() throws {
  if fromIPSW == nil && !linux {
    throw ValidationError("Please specify either a --from-ipsw or --linux option!")
  }
  #if arch(x86_64)
    if fromIPSW != nil {
      throw ValidationError("Only Linux VMs are supported on Intel!")
    }
  #endif
  if !diskFormat.isSupported {
    throw ValidationError("Disk format '\(diskFormat.rawValue)' is not supported.")
  }
}
```

실행 흐름:

```
Create.run()
  │
  1. VMDirectory.temporary() → 임시 디렉토리 생성
  │
  2. FileLock(lockURL: tmpVMDir.baseURL) → GC로부터 보호
  │
  3. withTaskCancellationHandler:
  │   │
  │   ├── macOS (--from-ipsw):
  │   │     "latest" → VZMacOSRestoreImage.fetchLatestSupported()
  │   │     URL → VM(vmDir:, ipswURL:, diskSizeGB:, diskFormat:)
  │   │
  │   └── Linux (--linux):
  │         VM.linux(vmDir:, diskSizeGB:, diskFormat:)
  │
  4. VMStorageLocal().move(name, from: tmpVMDir) → 최종 위치 배치
  │
  (onCancel: 임시 디렉토리 삭제)
```

**withTaskCancellationHandler 패턴**: Tart의 많은 커맨드가 이 패턴을 사용한다.
`operation` 클로저가 정상 실행되면 결과를 최종 위치에 배치하고, Ctrl+C(Task 취소)가
발생하면 `onCancel` 클로저가 임시 디렉토리를 정리한다. 이를 통해 최종 위치에는 항상
완전한 VM만 존재한다.

### 5.2 Run - VM 실행

소스 파일: `Sources/tart/Commands/Run.swift`

Run은 Tart에서 가장 복잡한 커맨드다 (약 700줄 이상). VM 실행, 네트워크 구성, 디스크
마운트, 디렉토리 공유, VNC, 시리얼 콘솔, GUI 등 모든 런타임 기능을 담당한다.

#### 옵션 구조

```swift
struct Run: AsyncParsableCommand {
  @Argument var name: String

  // UI/디스플레이
  @Flag var noGraphics: Bool = false
  @Flag var serial: Bool = false
  @Option var serialPath: String?
  @Flag var graphics: Bool = false
  @Flag var vnc: Bool = false
  @Flag var vncExperimental: Bool = false
  @Flag var captureSystemKeys: Bool = false

  // 오디오/클립보드
  @Flag var noAudio: Bool = false
  @Flag var noClipboard: Bool = false

  // 부트/하드웨어
  @Flag var recovery: Bool = false
  @Flag var nested: Bool = false
  @Flag var suspendable: Bool = false
  @Flag var noTrackpad: Bool = false
  @Flag var noPointer: Bool = false
  @Flag var noKeyboard: Bool = false

  // 스토리지
  @Option var disk: [String] = []
  @Option var rosettaTag: String?
  @Option var dir: [String] = []
  @Option var rootDiskOpts: String = ""

  // 네트워크
  @Option var netBridged: [String] = []
  @Flag var netSoftnet: Bool = false
  @Option var netSoftnetAllow: String?
  @Option var netSoftnetBlock: String?
  @Option var netSoftnetExpose: String?
  @Flag var netHost: Bool = false
```

#### 유효성 검증

`validate()` 메서드는 상호 배타적 옵션, macOS 버전 호환성, 아키텍처 제한 등을 검증한다:

```swift
mutating func validate() throws {
  // VNC 옵션 상호 배타
  if vnc && vncExperimental {
    throw ValidationError("--vnc and --vnc-experimental are mutually exclusive")
  }

  // Softnet 관련 옵션 → 자동 활성화
  if netSoftnetAllow != nil || netSoftnetBlock != nil || netSoftnetExpose != nil {
    netSoftnet = true
  }

  // 네트워크 모드 상호 배타
  var netFlags = 0
  if netBridged.count > 0 { netFlags += 1 }
  if netSoftnet { netFlags += 1 }
  if netHost { netFlags += 1 }
  if netFlags > 1 {
    throw ValidationError("--net-bridged, --net-softnet and --net-host are mutually exclusive")
  }

  // Suspended VM → 자동으로 suspendable 활성화
  let vmDir = try localStorage.open(name)
  if try vmDir.state() == .Suspended {
    suspendable = true
  }

  // Nested virtualization: macOS 15+ M3 이상
  if nested {
    if #unavailable(macOS 15) {
      throw ValidationError("Nested virtualization requires macOS 15 (Sequoia)")
    } else if !VZGenericPlatformConfiguration.isNestedVirtualizationSupported {
      throw ValidationError("Nested virtualization requires M3 chip or later")
    }
  }
}
```

#### 실행 흐름

```
Run.run() [@MainActor]
  │
  1. VMStorageLocal().open(name) → VM 디렉토리 열기
  │
  2. MAC 주소 충돌 검사
  │   전역 잠금 → 실행 중인 VM 목록 확인 → 충돌 시 regenerateMACAddress()
  │
  3. Softnet SUID 비트 확인 (대화형 세션인 경우)
  │
  4. 시리얼 포트 설정 (--serial / --serial-path)
  │
  5. VM 인스턴스 생성
  │   VM(vmDir:, network:, additionalStorageDevices:,
  │      directorySharingDevices:, serialPorts:, suspendable:,
  │      nested:, audio:, clipboard:, sync:, caching:, ...)
  │
  6. VNC 설정 (--vnc / --vnc-experimental)
  │
  7. PIDLock 획득 (config.json에 대한 fcntl 잠금)
  │   → 이미 잠겨있으면 "VM already running" 에러
  │
  8. Task { ... } 으로 VM 시작
  │   ├── 일시정지 상태 복원 (state.vzvmsave 존재 시)
  │   ├── vm!.start(recovery:, resume:)
  │   ├── VNC URL 출력/열기
  │   ├── ControlSocket 시작 (macOS 14+)
  │   └── vm!.run() → 종료 대기
  │
  9. 시그널 핸들러 등록
  │   SIGINT  → task.cancel()               (tart stop)
  │   SIGUSR1 → pause → snapshot → cancel   (tart suspend)
  │   SIGUSR2 → requestStop()               (graceful shutdown)
  │
  10. UI 모드 분기
       noGraphics → NSApplication.shared.run() (이벤트 루프만)
       그 외      → runUI() → MainApp (SwiftUI)
```

#### 시그널 처리 체계

```
┌──────────────────────────────────────────────────────────┐
│                tart run 시그널 핸들러                       │
├──────────────────────────────────────────────────────────┤
│                                                           │
│  SIGINT (Ctrl+C 또는 tart stop에서 kill)                   │
│  → task.cancel()                                          │
│  → VM의 withTaskCancellationHandler가 정리 수행             │
│                                                           │
│  SIGUSR1 (tart suspend에서 kill)                           │
│  → vm!.virtualMachine.pause()                              │
│  → vm!.virtualMachine.saveMachineStateTo(url: stateURL)    │
│  → task.cancel() (VM 종료)                                 │
│                                                           │
│  SIGUSR2 (graceful shutdown)                               │
│  → vm!.virtualMachine.requestStop()                        │
│  → macOS: 종료 확인 대화상자 표시                              │
│  → Linux: 즉시 종료                                         │
│                                                           │
└──────────────────────────────────────────────────────────┘
```

```swift
// SIGINT 핸들러
let sigintSrc = DispatchSource.makeSignalSource(signal: SIGINT)
sigintSrc.setEventHandler {
  task.cancel()
}
sigintSrc.activate()

// SIGUSR1 핸들러 (Suspend)
signal(SIGUSR1, SIG_IGN)
let sigusr1Src = DispatchSource.makeSignalSource(signal: SIGUSR1)
sigusr1Src.setEventHandler {
  Task {
    try vm!.configuration.validateSaveRestoreSupport()
    print("pausing VM to take a snapshot...")
    try await vm!.virtualMachine.pause()
    print("creating a snapshot...")
    try await vm!.virtualMachine.saveMachineStateTo(url: vmDir.stateURL)
    print("snapshot created successfully! shutting down the VM...")
    task.cancel()
  }
}
sigusr1Src.activate()

// SIGUSR2 핸들러 (Graceful shutdown)
signal(SIGUSR2, SIG_IGN)
let sigusr2Src = DispatchSource.makeSignalSource(signal: SIGUSR2)
sigusr2Src.setEventHandler {
  Task {
    print("Requesting guest OS to stop...")
    try vm!.virtualMachine.requestStop()
  }
}
sigusr2Src.activate()
```

### 5.3 Clone - VM 복제

소스 파일: `Sources/tart/Commands/Clone.swift`

```swift
struct Clone: AsyncParsableCommand {
  @Argument(help: "source VM name", completion: .custom(completeMachines))
  var sourceName: String

  @Argument(help: "new VM name")
  var newName: String

  @Flag var insecure: Bool = false
  @Option var concurrency: UInt = 4
  @Flag var deduplicate: Bool = false
  @Option var pruneLimit: UInt = 100
```

실행 흐름:

```
Clone.run()
  │
  1. 리모트 이름이고 OCI 캐시에 없으면 자동 Pull
  │   RemoteName(sourceName) → ociStorage.exists(remoteName)?
  │   → 없으면 ociStorage.pull(remoteName, registry, ...)
  │
  2. VMStorageHelper.open(sourceName) → 소스 VM 열기
  │   (로컬이면 VMStorageLocal, 리모트이면 VMStorageOCI)
  │
  3. VMDirectory.temporary() → 임시 디렉토리
  │
  4. withTaskCancellationHandler:
  │   │
  │   a. FileLock(lockURL: Config().tartHomeDir) → 전역 잠금
  │   │
  │   b. MAC 주소 충돌 검사
  │   │   hasVMsWithMACAddress() && state != .Suspended
  │   │   → 충돌 시 generateMAC = true
  │   │
  │   c. sourceVM.clone(to: tmpVMDir, generateMAC: generateMAC)
  │   │   → FileManager.copyItem() → APFS CoW 클론!
  │   │
  │   d. localStorage.move(newName, from: tmpVMDir)
  │   │
  │   e. 전역 잠금 해제
  │   │
  │   f. APFS CoW 미할당 공간 확보
  │      unallocatedBytes = sizeBytes - allocatedSizeBytes
  │      reclaimBytes = min(unallocatedBytes, pruneLimit * 1GB)
  │      Prune.reclaimIfNeeded(reclaimBytes)
  │
  (onCancel: 임시 디렉토리 삭제)
```

**왜 Suspended VM에서는 MAC을 재생성하지 않는가?** 일시정지된 VM의 상태
스냅샷(`state.vzvmsave`)에는 네트워크 디바이스의 MAC 주소가 포함되어 있다.
MAC을 변경하면 스냅샷과 불일치가 발생하여 복원에 실패할 수 있다.

**왜 전역 잠금이 필요한가?** MAC 주소 충돌 검사와 클론이 원자적이어야 한다.
두 `tart clone` 프로세스가 동시에 실행되면 같은 MAC 주소로 두 VM이 생성될 수 있다.

### 5.4 Pull - 레지스트리에서 VM 가져오기

소스 파일: `Sources/tart/Commands/Pull.swift`

```swift
func run() async throws {
  // 로컬 이미지를 지정한 경우 경고
  if try VMStorageLocal().exists(remoteName) {
    print("\"\(remoteName)\" is a local image, nothing to pull here!")
    return
  }

  let remoteName = try RemoteName(remoteName)
  let registry = try Registry(host: remoteName.host,
                                namespace: remoteName.namespace, insecure: insecure)

  defaultLogger.appendNewLine("pulling \(remoteName)...")

  try await VMStorageOCI().pull(remoteName, registry: registry,
                                 concurrency: concurrency, deduplicate: deduplicate)
}
```

Pull의 핵심 로직은 `VMStorageOCI.pull()`에 위임된다(09-vm-storage.md 참조).

**왜 로컬 이미지 확인을 먼저 하는가?** 사용자가 실수로 `tart pull my-local-vm`처럼
로컬 VM 이름을 입력할 수 있다. 이를 감지하여 불필요한 네트워크 요청을 방지한다.

### 5.5 Push - VM을 레지스트리에 업로드

소스 파일: `Sources/tart/Commands/Push.swift`

```swift
func run() async throws {
  let localVMDir = try VMStorageHelper.open(localName)
  let lock = try localVMDir.lock()
  if try !lock.trylock() {
    throw RuntimeError.VMIsRunning(localName)   // 실행 중인 VM은 Push 불가
  }

  // 레지스트리별로 그룹핑
  let registryGroups = Dictionary(grouping: remoteNames, by: {
    RegistryIdentifier(host: $0.host, namespace: $0.namespace)
  })

  for (registryIdentifier, remoteNamesForRegistry) in registryGroups {
    // 이미 레지스트리에 있는 OCI 이미지면 경량 Push (매니페스트만)
    if let remoteName = try? RemoteName(localName) {
      pushedRemoteName = try await lightweightPushToRegistry(...)
    } else {
      // 전체 Push: config + disk + NVRAM + manifest
      pushedRemoteName = try await localVMDir.pushToRegistry(...)

      // 로컬 캐시에도 저장 (--populate-cache)
      if populateCache {
        let expectedPushedVMDir = try ociStorage.create(pushedRemoteName)
        try localVMDir.clone(to: expectedPushedVMDir, generateMAC: false)
      }
    }
  }
}
```

**경량 Push**: 이미 OCI 캐시에 있는 이미지를 다른 태그로 Push하는 경우, 실제
데이터(config, disk, NVRAM)를 다시 업로드하지 않고 매니페스트만 Push한다.
레지스트리의 content-addressable storage 덕분에 같은 다이제스트의 blob은 이미 존재한다.

**--populate-cache**: Push 후 나중에 같은 이미지를 Pull할 것이 예상되면, 이 플래그로
로컬 OCI 캐시를 미리 채워둘 수 있다. 네트워크 트래픽을 절약한다.

### 5.6 Exec - VM 내부 명령 실행

소스 파일: `Sources/tart/Commands/Exec.swift`

Exec는 VM 내부의 Tart Guest Agent와 gRPC 양방향 스트리밍으로 통신하여 원격 명령을
실행한다. SSH와 유사하지만, Guest Agent와 Unix 도메인 소켓(vsock)으로 통신한다.

```swift
struct Exec: AsyncParsableCommand {
  @Flag(name: [.customShort("i")]) var interactive: Bool = false
  @Flag(name: [.customShort("t")]) var tty: Bool = false
  @Argument var name: String
  @Argument(parsing: .captureForPassthrough) var command: [String]
```

```
tart exec my-vm -- ls -la /tmp
  │
  1. VMStorageLocal().open(name)
  2. vmDir.running() 확인
  3. gRPC 채널 생성 (Unix 도메인 소켓 → control.sock)
  4. agentAsyncClient.makeExecCall() 양방향 스트림
  │
  ├── 요청 스트림:
  │   ├── command(name: "ls", args: ["-la", "/tmp"], interactive, tty)
  │   ├── standardInput(data: ...)   [interactive 모드]
  │   └── terminalResize(cols, rows) [tty 모드]
  │
  └── 응답 스트림:
      ├── standardOutput(data) → stdout에 출력
      ├── standardError(data) → stderr에 출력
      └── exit(code) → ExecCustomExitCodeError(exitCode: code)
```

**Unix 도메인 소켓 경로 제한 우회:**

```swift
// Unix 도메인 소켓은 최대 104바이트
// VM 디렉토리 경로가 길면 초과할 수 있으므로 상대 경로 사용
if let baseURL = vmDir.controlSocketURL.baseURL {
  FileManager.default.changeCurrentDirectoryPath(baseURL.path())
}

let channel = try GRPCChannelPool.with(
  target: .unixDomainSocket(vmDir.controlSocketURL.relativePath),
  // ...
)
```

**TTY 모드**: `-t` 플래그가 지정되면 호스트 터미널을 raw 모드로 전환하고,
SIGWINCH 시그널을 모니터링하여 터미널 크기 변경을 VM에 전달한다:

```swift
if tty && Term.IsTerminal() {
  state = try Term.MakeRaw()
}
defer {
  if let state { try! Term.Restore(state) }
}
```

### 5.7 Stop - VM 중지

소스 파일: `Sources/tart/Commands/Stop.swift`

```swift
func run() async throws {
  let vmDir = try VMStorageLocal().open(name)
  switch try vmDir.state() {
  case .Suspended:
    try stopSuspended(vmDir)     // state.vzvmsave 삭제
  case .Running:
    try await stopRunning(vmDir) // SIGINT → 대기 → SIGKILL
  case .Stopped:
    throw RuntimeError.VMNotRunning(name)
  }
}

func stopRunning(_ vmDir: VMDirectory) async throws {
  let lock = try vmDir.lock()
  var pid = try lock.pid()
  if pid == 0 { throw RuntimeError.VMNotRunning(name) }

  kill(pid, SIGINT)               // 1. 우아한 종료 요청

  // 2. timeout 초 동안 100ms 간격으로 종료 확인
  while gracefulWaitDuration.value > 0 {
    pid = try lock.pid()
    if pid == 0 { return }        // 종료됨
    try await Task.sleep(nanoseconds: 100_000_000)
    gracefulWaitDuration -= gracefulTickDuration
  }

  let ret = kill(pid, SIGKILL)    // 3. 강제 종료
  if ret != 0 {
    throw RuntimeError.VMTerminationFailed("...")
  }
}
```

```
Stop 흐름:

  tart stop my-vm [--timeout 30]
    │
    ├── Suspended 상태 → state.vzvmsave 삭제 → Stopped
    │
    ├── Running 상태:
    │     1. PIDLock에서 tart run의 PID 조회
    │     2. kill(pid, SIGINT) → 우아한 종료 요청
    │     3. 100ms 간격으로 PID 확인 (최대 30초)
    │     4. 시간 초과 → kill(pid, SIGKILL) → 강제 종료
    │
    └── Stopped 상태 → VMNotRunning 에러
```

### 5.8 Suspend - VM 일시중지

소스 파일: `Sources/tart/Commands/Suspend.swift`

```swift
func run() async throws {
  let vmDir = try VMStorageLocal().open(name)
  let lock = try vmDir.lock()
  let pid = try lock.pid()
  if pid == 0 {
    throw RuntimeError.VMNotRunning("VM \"\(name)\" is not running")
  }

  // "tart run" 프로세스에 SIGUSR1 전송
  let ret = kill(pid, SIGUSR1)
  if ret != 0 {
    throw RuntimeError.SuspendFailed("failed to send SIGUSR1 signal ...")
  }
}
```

**왜 직접 VM 상태를 저장하지 않는가?** VM 객체는 `tart run` 프로세스가 소유하고 있다.
`tart suspend`는 별도 프로세스이므로 VM 객체에 직접 접근할 수 없다. 대신 PID 잠금에서
`tart run`의 PID를 읽어 SIGUSR1을 보내면, `tart run`의 SIGUSR1 핸들러가
`pause() → saveMachineStateTo() → cancel()` 시퀀스를 실행한다.

### 5.9 Set/Get - VM 설정 변경/조회

소스 파일: `Sources/tart/Commands/Set.swift`, `Sources/tart/Commands/Get.swift`

```swift
// Set
struct Set: AsyncParsableCommand {
  @Argument var name: String
  @Option var cpu: UInt16?
  @Option var memory: UInt64?           // MB 단위
  @Option var display: VMDisplayConfig?
  @Flag var displayRefit: Bool? = nil
  @Flag var randomMAC: Bool = false
  @Flag var randomSerial: Bool = false   // arm64 only
  @Option var disk: String?             // 디스크 이미지 교체
  @Option var diskSize: UInt16?         // 디스크 리사이즈 (GB)

  func run() async throws {
    let vmDir = try VMStorageLocal().open(name)
    var vmConfig = try VMConfig(fromURL: vmDir.configURL)

    if let cpu = cpu { try vmConfig.setCPU(cpuCount: Int(cpu)) }
    if let memory = memory { try vmConfig.setMemory(memorySize: memory * 1024 * 1024) }
    // ... 설정 적용
    try vmConfig.save(toURL: vmDir.configURL)

    // 디스크 교체: 임시 복사 후 원자적 교체
    if let disk = disk {
      let temporaryDiskURL = try Config().tartTmpDir
        .appendingPathComponent("set-disk-\(UUID().uuidString)")
      try FileManager.default.copyItem(atPath: disk, toPath: temporaryDiskURL.path())
      _ = try FileManager.default.replaceItemAt(vmDir.diskURL, withItemAt: temporaryDiskURL)
    }

    if diskSize != nil { try vmDir.resizeDisk(diskSize!) }
  }
}
```

```swift
// Get
struct Get: AsyncParsableCommand {
  @Argument var name: String
  @Option var format: Format = .text

  func run() async throws {
    let vmDir = try VMStorageLocal().open(name)
    let vmConfig = try VMConfig(fromURL: vmDir.configURL)
    let memorySizeInMb = vmConfig.memorySize / 1024 / 1024

    let info = VMInfo(OS: vmConfig.os, CPU: vmConfig.cpuCount,
                       Memory: memorySizeInMb, Disk: try vmDir.sizeGB(),
                       DiskFormat: vmConfig.diskFormat.rawValue,
                       Size: String(format: "%.3f",
                         Float(try vmDir.allocatedSizeBytes()) / 1000 / 1000 / 1000),
                       Display: vmConfig.display.description,
                       Running: try vmDir.running(),
                       State: try vmDir.state().rawValue)
    print(format.renderSingle(info))
  }
}
```

### 5.10 List - VM 목록 조회

소스 파일: `Sources/tart/Commands/List.swift`

```swift
struct List: AsyncParsableCommand {
  @Option var source: String?             // "local" 또는 "oci"
  @Option var format: Format = .text      // "text" 또는 "json"
  @Flag(name: [.short, .long]) var quiet: Bool = false

  func run() async throws {
    var infos: [VMInfo] = []

    if source == nil || source == "local" {
      infos += sortedInfos(try VMStorageLocal().list().map { (name, vmDir) in
        try VMInfo(Source: "local", Name: name, Disk: vmDir.sizeGB(),
                   Size: vmDir.allocatedSizeGB(), ...)
      })
    }

    if source == nil || source == "oci" {
      infos += sortedInfos(try VMStorageOCI().list().map { (name, vmDir, _) in
        try VMInfo(Source: "OCI", Name: name, Disk: vmDir.sizeGB(),
                   Size: vmDir.allocatedSizeGB(), ...)
      })
    }

    if (quiet) {
      for info in infos { print(info.Name) }
    } else {
      print(format.renderList(infos))
    }
  }
}
```

`--source local`/`--source oci`로 필터링 가능. `--quiet`로 이름만 출력.
`--format json`으로 JSON 출력. 기본적으로 로컬과 OCI를 모두 표시한다.

### 5.11 기타 커맨드 요약

| 커맨드 | 핵심 동작 |
|--------|----------|
| **Delete** | `VMStorageHelper.delete(name)` - 로컬/리모트 자동 판별 후 삭제. 다중 이름 지원(`@Argument var name: [String]`) |
| **Import** | 임시 디렉토리 생성 → `importFromArchive(path:)` → MAC 충돌 검사 → `move()`. `.tvm` 압축 파일 지원 |
| **Export** | `VMStorageHelper.open(name).exportToArchive(path:)` → `.tvm` 압축 파일 생성. 경로 미지정 시 `{name}.tvm` |
| **Rename** | `VMStorageLocal().rename(name, newName)`. 로컬 VM만 가능. 대상 이름에 `/` 포함 불가 |
| **Login** | 레지스트리 인증. `--username` + `--password-stdin` 또는 대화형 입력. `KeychainCredentialsProvider().store()` |
| **Logout** | `KeychainCredentialsProvider().remove(host:)` |
| **FQN** | OCI 이미지의 정규화된 이름(Fully Qualified Name) 출력. 태그 → 다이제스트 변환. `shouldDisplay: false` (숨김) |
| **IP** | MAC 주소 기반 IP 조회. `--resolver={dhcp,arp,agent}`. `--wait` 옵션으로 부팅 대기 |
| **Prune** | OCI 캐시/IPSW 캐시/로컬 VM 정리. `--older-than`, `--space-budget`, `--entries={caches,vms}` |

## 6. withTaskCancellationHandler 패턴

Tart의 커맨드에서 반복적으로 사용되는 핵심 패턴:

```swift
// Create, Clone, Import, Pull에서 반복되는 패턴
let tmpVMDir = try VMDirectory.temporary()
let tmpVMDirLock = try FileLock(lockURL: tmpVMDir.baseURL)
try tmpVMDirLock.lock()

try await withTaskCancellationHandler(operation: {
  // 정상 경로: 임시 디렉토리에서 작업 수행
  // ...
  // 완료 시 최종 위치로 원자적 이동
  try VMStorageLocal().move(name, from: tmpVMDir)
}, onCancel: {
  // 취소 경로: 임시 디렉토리 정리
  try? FileManager.default.removeItem(at: tmpVMDir.baseURL)
})
```

```
┌─────────────────────────────────────────────────────────┐
│          withTaskCancellationHandler 패턴                 │
├─────────────────────────────────────────────────────────┤
│                                                          │
│  1. 임시 디렉토리 생성 (UUID 또는 MD5 해시)                │
│  2. FileLock 획득 → GC로부터 보호                         │
│  3. operation 클로저:                                     │
│     a. 임시 디렉토리에서 VM 생성/다운로드/복제               │
│     b. 성공 시 FileManager.replaceItemAt()으로 원자적 배치  │
│  4. onCancel 클로저:                                      │
│     a. Ctrl+C → Task.cancel() → onCancel 실행            │
│     b. 임시 디렉토리 삭제 → 불완전한 데이터 정리             │
│                                                          │
│  결과: 최종 위치에는 항상 완전한 VM만 존재                   │
└─────────────────────────────────────────────────────────┘
```

사용하는 커맨드: `Create`, `Clone`, `Import`, `Pull`(VMStorageOCI.pull 내부)

## 7. 셸 자동완성

swift-argument-parser의 커스텀 완성 기능을 활용한다:

```swift
// Run.swift
@Argument(help: "VM name", completion: .custom(completeLocalMachines))
var name: String

// Clone.swift
@Argument(help: "source VM name", completion: .custom(completeMachines))
var sourceName: String

// Stop.swift, Suspend.swift
@Argument(help: "VM name", completion: .custom(completeRunningMachines))
var name: String
```

| 완성 함수 | 대상 | 사용 커맨드 |
|-----------|------|-----------|
| `completeLocalMachines` | 로컬 VM 이름만 | Run, Set, Get, Rename, Exec |
| `completeMachines` | 로컬 + OCI VM 이름 | Clone, Delete, Export |
| `completeRunningMachines` | 실행 중인 VM 이름만 | Stop, Suspend |

## 8. 커맨드 파싱 흐름

```
사용자 입력: tart clone ghcr.io/org/macos:latest my-vm --concurrency 8

  1. Root.main() 호출

  2. parseAsRoot()
     │
     ├── 첫 번째 인자 "clone" → Clone 서브커맨드 매칭
     │
     ├── @Argument sourceName = "ghcr.io/org/macos:latest"
     │
     ├── @Argument newName = "my-vm"
     │
     └── @Option concurrency = 8

  3. Clone.validate() 호출
     │
     ├── newName에 "/" 없음 → OK
     │
     └── concurrency >= 1 → OK

  4. Clone.run() 호출
     │
     └── 비동기 실행 (await)
```

swift-argument-parser의 프로퍼티 래퍼가 자동으로 처리하는 것들:

| 프로퍼티 래퍼 | 역할 | 예시 |
|-------------|------|------|
| `@Argument` | 위치 기반 인자 | `tart clone SOURCE DEST` |
| `@Option` | 이름 기반 옵션 | `--concurrency 8` |
| `@Flag` | 불리언 플래그 | `--insecure` |
| `completion:` | 셸 자동완성 | `.custom(completeMachines)` |
| `help:` | 도움말 텍스트 | `--help` 출력에 표시 |

## 9. OpenTelemetry 통합

모든 커맨드 실행은 OpenTelemetry 스팬으로 추적된다:

```swift
// Root.swift
let span = OTel.shared.tracer.spanBuilder(
  spanName: type(of: command)._commandName).startSpan()
defer { span.end() }
OpenTelemetry.instance.contextProvider.setActiveSpan(span)

// 커맨드라인 인자 기록
span.setAttribute(key: "Command-line arguments",
                   value: .array(AttributeArray(values: commandLineArguments)))

// Cirrus CI 태그 기록
if let tags = ProcessInfo.processInfo.environment["CIRRUS_SENTRY_TAGS"] {
  for (key, value) in tags.split(separator: ",").compactMap(splitEnvironmentVariable) {
    span.setAttribute(key: key, value: .string(value))
  }
}
```

에러 발생 시에도 스팬에 기록된다:

```swift
// Root.swift 에러 핸들러
OpenTelemetry.instance.contextProvider.activeSpan?.recordException(error)
```

Pull 등의 서브스팬도 생성된다:

```swift
// VMStorageOCI.swift
let span = OTel.shared.tracer.spanBuilder(spanName: "pull").setActive(true).startSpan()
defer { span.end() }
```

**왜 OpenTelemetry인가?** Cirrus Labs(Tart 개발사)가 프로덕션 환경에서 Tart의
성능과 안정성을 모니터링하기 위해 사용한다. Pull 시간, 에러율, 디스크 사용량 등을
추적할 수 있다.

## 10. 에러 타입 계층

```swift
// Sources/tart/VMStorageHelper.swift
enum RuntimeError : Error {
  case Generic(_ message: String)
  case VMConfigurationError(_ message: String)
  case VMDoesNotExist(name: String)
  case VMMissingFiles(_ message: String)
  case VMIsRunning(_ name: String)
  case VMNotRunning(_ name: String)
  case VMAlreadyRunning(_ message: String)
  case NoIPAddressFound(_ message: String)
  case DiskAlreadyInUse(_ message: String)
  case FailedToOpenBlockDevice(_ path: String, _ explanation: String)
  case InvalidDiskSize(_ message: String)
  case FailedToCreateDisk(_ message: String)
  case FailedToResizeDisk(_ message: String)
  case FailedToUpdateAccessDate(_ message: String)
  case PIDLockFailed(_ message: String)
  case PIDLockMissing(_ message: String)
  case FailedToParseRemoteName(_ message: String)
  case VMTerminationFailed(_ message: String)
  case ImproperlyFormattedHost(_ host: String, _ hint: String)
  case InvalidCredentials(_ message: String)
  case VMDirectoryAlreadyInitialized(_ message: String)
  case ExportFailed(_ message: String)
  case ImportFailed(_ message: String)
  case SoftnetFailed(_ message: String)
  case OCIStorageError(_ message: String)
  case SuspendFailed(_ message: String)
  case PullFailed(_ message: String)
  case VirtualMachineLimitExceeded(_ hint: String)
  case VMSocketFailed(_ port: UInt32, _ explanation: String)
  case TerminalOperationFailed(_ message: String)
}
```

```swift
// Softnet 전용 에러
enum SoftnetError: Error {
  case InitializationFailed(why: String)
  case RuntimeFailed(why: String)
}

// Exec 전용 에러 (종료 코드 전달)
struct ExecCustomExitCodeError: Error {
  let exitCode: Int32
}
```

| 에러 카테고리 | 종료 코드 | 예시 |
|-------------|---------|------|
| VM 상태 에러 | 2 | VMDoesNotExist, VMNotRunning, VMAlreadyRunning |
| 일반 에러 | 1 | 그 외 RuntimeError |
| Exec 원격 종료 코드 | N | 원격 명령의 종료 코드 그대로 전달 |
| ArgumentParser 에러 | 64 | 잘못된 인자, 유효성 검증 실패 |

## 11. 설계 결정 요약

| 설계 결정 | 선택 | 이유 |
|----------|------|------|
| AsyncParsableCommand | 모든 커맨드가 async | VM Pull/Push, 네트워크 I/O 등 핵심 작업이 비동기 |
| signal(SIGINT, SIG_IGN) + DispatchSource | Task.cancel()로 통합 | 기본 핸들러와의 경쟁 방지, 구조적 동시성에 통합 |
| Pull/Clone에서 GC 건너뜀 | 자체 프루닝으로 대체 | 이전 시도의 부분 다운로드 데이터 보존 |
| 커맨드별 종료 코드 | 2=VM 상태, 1=일반 | 스크립트에서 에러 유형 구분 가능 |
| SIGUSR1 기반 Suspend | Run에 시그널 전송 | VM 소유권이 Run 프로세스에 있으므로 |
| SIGUSR2 기반 Graceful | requestStop() | macOS VM에서 종료 확인 대화상자 표시 |
| 임시 디렉토리 + 원자적 이동 | replaceItemAt() | 불완전한 VM이 최종 위치에 나타나지 않음 |
| Suspend 조건부 등록 | #available(macOS 14) | 미지원 OS에서 커맨드 자체를 숨김 |
| setlinebuf(stdout) | 라인 버퍼링 | 파이프 연결 시에도 실시간 출력 |
| Unix 소켓 경로 우회 | changeCurrentDirectoryPath | 104바이트 경로 제한 대응 |
| ExecCustomExitCodeError | 원격 종료 코드 전달 | SSH와 유사한 동작 |

## 12. 소스 파일 참조

| 파일 | 경로 | 역할 |
|------|------|------|
| Root.swift | `Sources/tart/Root.swift` | @main 진입점, 서브커맨드 등록, 시그널 처리 |
| Create.swift | `Sources/tart/Commands/Create.swift` | VM 생성 (macOS/Linux) |
| Clone.swift | `Sources/tart/Commands/Clone.swift` | VM 복제 (APFS CoW) |
| Run.swift | `Sources/tart/Commands/Run.swift` | VM 실행 (GUI/headless/VNC) |
| Stop.swift | `Sources/tart/Commands/Stop.swift` | VM 중지 (graceful/forceful) |
| Suspend.swift | `Sources/tart/Commands/Suspend.swift` | VM 일시중지 (macOS 14+) |
| Exec.swift | `Sources/tart/Commands/Exec.swift` | VM 내부 명령 실행 (gRPC) |
| Pull.swift | `Sources/tart/Commands/Pull.swift` | OCI 레지스트리에서 Pull |
| Push.swift | `Sources/tart/Commands/Push.swift` | OCI 레지스트리에 Push |
| Set.swift | `Sources/tart/Commands/Set.swift` | VM 설정 변경 |
| Get.swift | `Sources/tart/Commands/Get.swift` | VM 설정 조회 |
| List.swift | `Sources/tart/Commands/List.swift` | VM 목록 조회 |
| Delete.swift | `Sources/tart/Commands/Delete.swift` | VM 삭제 |
| Import.swift | `Sources/tart/Commands/Import.swift` | .tvm 파일에서 VM 가져오기 |
| Export.swift | `Sources/tart/Commands/Export.swift` | VM을 .tvm 파일로 내보내기 |
| Rename.swift | `Sources/tart/Commands/Rename.swift` | 로컬 VM 이름 변경 |
| Login.swift | `Sources/tart/Commands/Login.swift` | 레지스트리 인증 |
| Logout.swift | `Sources/tart/Commands/Logout.swift` | 레지스트리 인증 해제 |
| IP.swift | `Sources/tart/Commands/IP.swift` | VM IP 주소 조회 |
| Prune.swift | `Sources/tart/Commands/Prune.swift` | 캐시/VM 정리 |
| FQN.swift | `Sources/tart/Commands/FQN.swift` | 정규화된 이름 조회 |
| Package.swift | `Package.swift` | swift-argument-parser 의존성 |
| VMStorageHelper.swift | `Sources/tart/VMStorageHelper.swift` | RuntimeError 정의, 종료 코드 |
