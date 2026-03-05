# 10. 네트워크 시스템 심화

## 1. 개요

Tart의 네트워크 시스템은 macOS/Linux 가상 머신에 네트워크 연결을 제공하는 인프라다.
Apple의 Virtualization.Framework가 제공하는 네트워크 디바이스 어태치먼트 API 위에
3가지 네트워크 모드를 `Network` 프로토콜로 추상화하여 제공한다.

### 해결해야 할 핵심 과제

1. **간편한 기본 네트워크**: 대부분의 사용자는 별도 설정 없이 인터넷에 접속해야 한다
2. **브리지 네트워크**: 호스트와 같은 물리 네트워크에 VM을 직접 연결해야 한다
3. **네트워크 격리**: CI/CD 환경에서 VM 간 트래픽 격리와 보안이 필요하다
4. **DHCP 리스 관리**: 수백 개의 일시적 VM을 운영할 때 DHCP 주소 고갈을 방지해야 한다
5. **IP 주소 조회**: VM의 IP를 프로그래밍 방식으로 조회할 수 있어야 한다

### 왜 3가지 모드인가?

단일 네트워크 모드로는 모든 시나리오를 만족시킬 수 없다.

- **개발자가 로컬에서 VM을 실행할 때**: 최대한 간단하게, NAT로 충분하다
- **CI/CD에서 호스트 네트워크에 VM을 노출해야 할 때**: 브리지가 필요하다
- **CI/CD에서 VM을 격리 운영할 때**: Softnet의 패킷 필터링이 필요하다

```
┌───────────────────────────────────────────────────────┐
│                    Run 커맨드                           │
│  --net-bridged / --net-softnet / --net-host / (기본)   │
└───────────────────┬───────────────────────────────────┘
                    │ userSpecifiedNetwork()
                    ▼
          ┌─────────────────┐
          │ Network 프로토콜  │
          │  attachments()   │
          │  run()           │
          │  stop()          │
          └────────┬────────┘
       ┌───────────┼───────────┐
       ▼           ▼           ▼
┌──────────┐ ┌──────────┐ ┌──────────┐
│ Network  │ │ Network  │ │ Softnet  │
│ Shared   │ │ Bridged  │ │          │
│ (NAT)    │ │          │ │ (격리)    │
└──────────┘ └──────────┘ └──────────┘
     │            │            │
     ▼            ▼            ▼
┌──────────┐ ┌──────────┐ ┌──────────┐
│ VZNAT    │ │ VZBridged│ │ VZFile   │
│ Network  │ │ Network  │ │ Handle   │
│ Device   │ │ Device   │ │ Network  │
│Attachment│ │Attachment│ │ Device   │
└──────────┘ └──────────┘ │Attachment│
                           └──────────┘
                                │
                           socketpair(2)
                                │
                           softnet 프로세스
```

## 2. Network 프로토콜

소스 파일: `Sources/tart/Network/Network.swift`

```swift
import Virtualization
import Semaphore

protocol Network {
  func attachments() -> [VZNetworkDeviceAttachment]
  func run(_ sema: AsyncSemaphore) throws
  func stop() async throws
}
```

| 메서드 | 역할 |
|--------|------|
| `attachments()` | Virtualization.Framework에 전달할 네트워크 디바이스 어태치먼트 배열 반환 |
| `run(_ sema:)` | 네트워크 서비스 시작 (Softnet에서만 실질적 동작) |
| `stop()` | 네트워크 서비스 중지 (Softnet에서만 실질적 동작) |

**왜 `AsyncSemaphore`를 사용하는가?** Softnet 프로세스가 비정상 종료하면 VM도 함께
중지되어야 한다. `run()` 메서드에 전달된 세마포어를 Softnet 모니터 태스크가 시그널하면,
VM 실행 루프가 이를 감지하고 종료 처리를 시작한다. Shared와 Bridged는 별도 프로세스가
없으므로 `run()`과 `stop()`이 no-op이다.

### 왜 프로토콜 추상화인가?

VM 객체는 네트워크 모드가 무엇이든 동일한 `Network` 인터페이스를 통해 어태치먼트를 받고,
시작/중지를 관리한다. 이를 통해:

1. VM 초기화 코드에서 네트워크 모드별 분기가 불필요하다
2. 새로운 네트워크 모드를 추가할 때 `Network` 프로토콜만 구현하면 된다
3. 테스트에서 모의(mock) 네트워크를 주입할 수 있다

## 3. NetworkShared - NAT 네트워크

소스 파일: `Sources/tart/Network/NetworkShared.swift`

```swift
class NetworkShared: Network {
  func attachments() -> [VZNetworkDeviceAttachment] {
    [VZNATNetworkDeviceAttachment()]
  }

  func run(_ sema: AsyncSemaphore) throws {
    // no-op, only used for Softnet
  }

  func stop() async throws {
    // no-op, only used for Softnet
  }
}
```

### 동작 원리

`VZNATNetworkDeviceAttachment`는 Virtualization.Framework가 제공하는 가장 간단한
네트워크 어태치먼트다. macOS 내장 NAT를 통해 VM에 사설 IP를 할당하고, 호스트의
인터넷 연결을 공유한다.

```
┌──────────────────────────────────────────┐
│           호스트 macOS                     │
│                                           │
│  ┌─────────┐    ┌──────────────────────┐ │
│  │   VM    │    │  macOS NAT 서비스     │ │
│  │192.168.│───►│  (vmnet.framework)    │──────► 인터넷
│  │ 64.x   │    │  DHCP + NAT + DNS    │ │
│  └─────────┘    └──────────────────────┘ │
│                                           │
└──────────────────────────────────────────┘
```

| 특성 | 설명 |
|------|------|
| IP 할당 | macOS DHCP 서버가 192.168.64.x 대역에서 자동 할당 |
| 인터넷 접근 | NAT를 통해 호스트의 네트워크 연결 공유 |
| 호스트에서 VM 접근 | DHCP 리스 파일에서 IP 조회 (`tart ip --resolver dhcp`) |
| VM 간 접근 | 같은 NAT 네트워크 내에서 가능 |
| 외부에서 VM 접근 | 불가 (NAT 뒤에 있으므로) |

**왜 기본 모드인가?** 가장 설정이 간단하고, 별도 권한이 필요 없으며, 대부분의 개발/테스트
시나리오에서 충분하다. Run 커맨드에서 네트워크 옵션을 지정하지 않으면 자동으로 `NetworkShared`가
선택된다:

```swift
// Sources/tart/Commands/Run.swift
vm = try VM(
  vmDir: vmDir,
  network: userSpecifiedNetwork(vmDir: vmDir) ?? NetworkShared(),
  // ...
)
```

### IP 조회 메커니즘 (DHCP)

NAT 모드에서 VM의 IP를 조회하려면 macOS의 DHCP 리스 파일을 파싱한다:

```swift
// Sources/tart/Commands/IP.swift
case .dhcp:
  if let leases = try Leases(),
     let ip = leases.ResolveMACAddress(macAddress: vmMACAddress) {
    return ip
  }
```

macOS의 DHCP 서버는 `/var/db/dhcpd_leases` 파일에 리스 정보를 기록한다. VM의 MAC 주소와
일치하는 엔트리를 찾으면 해당 IP를 반환한다.

## 4. NetworkBridged - 브리지 네트워크

소스 파일: `Sources/tart/Network/NetworkBridged.swift`

```swift
class NetworkBridged: Network {
  let interfaces: [VZBridgedNetworkInterface]

  init(interfaces: [VZBridgedNetworkInterface]) {
    self.interfaces = interfaces
  }

  func attachments() -> [VZNetworkDeviceAttachment] {
    interfaces.map { VZBridgedNetworkDeviceAttachment(interface: $0) }
  }

  func run(_ sema: AsyncSemaphore) throws {
    // no-op, only used for Softnet
  }

  func stop() async throws {
    // no-op, only used for Softnet
  }
}
```

### 동작 원리

브리지 네트워크는 VM을 호스트의 물리 네트워크 인터페이스에 직접 연결한다. VM은
호스트와 같은 서브넷에서 IP를 할당받고, 외부 네트워크에서 VM에 직접 접근할 수 있다.

```
┌──────────────────────────────────────────────────────┐
│           호스트 macOS                                 │
│                                                       │
│  ┌─────────┐    ┌───────────────┐    ┌─────────────┐ │
│  │   VM    │    │  브리지        │    │ 물리 NIC    │ │
│  │10.0.1.x│───►│  (en0 등)     │───►│ (Wi-Fi/Eth) │──► LAN
│  └─────────┘    └───────────────┘    └─────────────┘ │
│                                                       │
│  ┌─────────┐                                          │
│  │ 호스트   │─────────────────────────────────────────┘
│  │10.0.1.y│
│  └─────────┘
└──────────────────────────────────────────────────────┘
```

| 특성 | 설명 |
|------|------|
| IP 할당 | 물리 네트워크의 DHCP 서버에서 할당 |
| 인터넷 접근 | 물리 네트워크를 통해 직접 접근 |
| 호스트에서 VM 접근 | 같은 서브넷이므로 직접 접근 가능 |
| VM 간 접근 | 같은 물리 네트워크에서 직접 접근 가능 |
| 외부에서 VM 접근 | 물리 네트워크 구성에 따라 가능 |

### 인터페이스 선택

Run 커맨드에서 `--net-bridged` 옵션으로 브리지할 인터페이스를 지정한다:

```swift
// Sources/tart/Commands/Run.swift
@Option(help: ArgumentHelp("""
Use bridged networking instead of the default shared (NAT) networking
(e.g. --net-bridged=en0 or --net-bridged=\"Wi-Fi\")
"""))
var netBridged: [String] = []
```

인터페이스 이름(예: `en0`)이나 표시 이름(예: `Wi-Fi`)으로 지정할 수 있다:

```swift
// Sources/tart/Commands/Run.swift (userSpecifiedNetwork 내부)
func findBridgedInterface(_ name: String) throws -> VZBridgedNetworkInterface {
  let interface = VZBridgedNetworkInterface.networkInterfaces.first { interface in
    interface.identifier == name || interface.localizedDisplayName == name
  }
  if (interface == nil) {
    throw ValidationError("no bridge interfaces matched \"\(netBridged)\", "
      + "available interfaces: \(bridgeInterfaces())")
  }
  return interface!
}
```

**왜 배열로 여러 인터페이스를 지원하는가?** `--net-bridged`를 여러 번 지정하면 VM에 여러
네트워크 인터페이스를 추가할 수 있다. 이는 멀티-홈 네트워크 구성이나 관리 네트워크와
데이터 네트워크를 분리하는 시나리오에서 유용하다.

### IP 조회 메커니즘 (ARP)

브리지 모드에서는 macOS의 DHCP 리스 파일에 VM 정보가 없으므로, ARP 테이블을 파싱해야 한다:

```swift
// Sources/tart/Commands/IP.swift
case .arp:
  if let ip = try ARPCache().ResolveMACAddress(macAddress: vmMACAddress) {
    return ip
  }
```

ARP 리졸버는 외부 `arp` 명령의 출력을 파싱하여 VM의 MAC 주소에 대응하는 IP를 찾는다.
단, VM이 충분한 네트워크 활동을 발생시켜 호스트의 ARP 테이블에 등록되어야 한다.

## 5. Softnet - 소프트웨어 네트워크 격리

소스 파일: `Sources/tart/Network/Softnet.swift`

Softnet은 Tart 네트워크 시스템에서 가장 복잡하고 가장 강력한 모드다. 외부 `softnet`
프로세스를 서브프로세스로 실행하고, Unix 도메인 소켓페어를 통해 VM과 통신한다.

### 왜 Softnet이 필요한가?

CI/CD 환경에서 macOS의 기본 NAT는 여러 문제를 야기한다:

1. **DHCP 리스 고갈**: 기본 리스 시간이 86,400초(1일)여서, 하루 동안 수백 개의
   일시적 VM을 생성하면 주소가 고갈된다
2. **VM 간 격리 부재**: NAT 네트워크 내에서 VM끼리 자유롭게 통신할 수 있어 보안 위험이 있다
3. **ARP 스푸핑**: 악의적인 VM이 ARP 스푸핑으로 다른 VM의 트래픽을 가로챌 수 있다

Softnet은 유저스페이스 패킷 필터로 이러한 문제를 해결한다.

### 아키텍처 상세

```
┌──────────────────────────────────────────────────────────────┐
│                      호스트 macOS                              │
│                                                               │
│  ┌───────────┐  socketpair(2)  ┌──────────────┐              │
│  │    VM     │  AF_UNIX        │   softnet    │              │
│  │           │  SOCK_DGRAM     │   프로세스     │              │
│  │           │◄───────────────►│              │              │
│  │           │   vmFD - softnetFD              │              │
│  │           │                 │  패킷 필터:    │───► 인터넷    │
│  │           │                 │  - MAC 검증   │              │
│  │           │                 │  - IP 검증    │              │
│  │           │                 │  - CIDR 제어  │              │
│  │           │                 │  - 포트 노출   │              │
│  └───────────┘                 └──────────────┘              │
│                                      │                        │
│                                      │ SUID root              │
│                                      │ (vmnet 접근 권한)        │
│                                      ▼                        │
│                               ┌──────────────┐               │
│                               │ vmnet bridge │               │
│                               │ (bridge100)  │               │
│                               └──────────────┘               │
│                                                               │
└──────────────────────────────────────────────────────────────┘
```

### 초기화: socketpair 생성

```swift
class Softnet: Network {
  private let process = Process()
  private var monitorTask: Task<Void, Error>? = nil
  private let monitorTaskFinished = ManagedAtomic<Bool>(false)

  let vmFD: Int32

  init(vmMACAddress: String, extraArguments: [String] = []) throws {
    let fds = UnsafeMutablePointer<Int32>.allocate(capacity: MemoryLayout<Int>.stride * 2)

    let ret = socketpair(AF_UNIX, SOCK_DGRAM, 0, fds)
    if ret != 0 {
      throw SoftnetError.InitializationFailed(why: "socketpair() failed with exit code \(ret)")
    }

    vmFD = fds[0]
    let softnetFD = fds[1]

    try setSocketBuffers(vmFD, 1 * 1024 * 1024);
    try setSocketBuffers(softnetFD, 1 * 1024 * 1024);

    process.executableURL = try Self.softnetExecutableURL()
    process.arguments = ["--vm-fd", String(STDIN_FILENO),
                         "--vm-mac-address", vmMACAddress] + extraArguments
    process.standardInput = FileHandle(fileDescriptor: softnetFD, closeOnDealloc: false)
  }
}
```

**socketpair(2)의 역할:**

```
socketpair(AF_UNIX, SOCK_DGRAM, 0, fds)

fds[0] = vmFD      ──► VM에 연결 (VZFileHandleNetworkDeviceAttachment)
fds[1] = softnetFD  ──► softnet 프로세스의 stdin으로 전달

VM이 보내는 모든 이더넷 프레임:
  VM → vmFD → Unix 소켓 → softnetFD → softnet 프로세스

softnet이 보내는 모든 이더넷 프레임:
  softnet 프로세스 → softnetFD → Unix 소켓 → vmFD → VM
```

**왜 `AF_UNIX, SOCK_DGRAM`인가?** 이더넷 프레임은 메시지 경계가 명확한 데이터그램이다.
`SOCK_STREAM`을 사용하면 프레임 경계를 별도로 관리해야 하지만, `SOCK_DGRAM`은 각 send/recv가
하나의 완전한 프레임을 전달한다.

### softnet 프로세스 탐색

```swift
static func softnetExecutableURL() throws -> URL {
  let binaryName = "softnet"

  guard let executableURL = resolveBinaryPath(binaryName) else {
    throw SoftnetError.InitializationFailed(why: "\(binaryName) not found in PATH")
  }

  return executableURL
}
```

`resolveBinaryPath()`는 `PATH` 환경변수에서 `softnet` 바이너리를 검색한다. 일반적으로
Homebrew를 통해 설치된다 (`brew install cirruslabs/cli/softnet`).

### 소켓 버퍼 최적화

```swift
private func setSocketBuffers(_ fd: Int32, _ sizeBytes: Int) throws {
  let option_len = socklen_t(MemoryLayout<Int>.size)

  // Apple 권장: SO_RCVBUF는 SO_SNDBUF의 4배
  var receiveBufferSize = 4 * sizeBytes
  var ret = setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &receiveBufferSize, option_len)
  if ret != 0 {
    throw SoftnetError.InitializationFailed(why: "setsockopt(SO_RCVBUF) returned \(ret)")
  }

  var sendBufferSize = sizeBytes
  ret = setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &sendBufferSize, option_len)
  if ret != 0 {
    throw SoftnetError.InitializationFailed(why: "setsockopt(SO_SNDBUF) returned \(ret)")
  }
}
```

Apple의 `VZFileHandleNetworkDeviceAttachment` 문서에 따르면, 최적 성능을 위해
`SO_RCVBUF`는 `SO_SNDBUF`의 4배를 권장한다. Tart는 송신 버퍼 1MB, 수신 버퍼 4MB로 설정한다.

| 버퍼 | 크기 | 이유 |
|------|------|------|
| SO_SNDBUF | 1MB | VM에서 네트워크로 나가는 패킷의 기본 버퍼 |
| SO_RCVBUF | 4MB | 네트워크에서 VM으로 들어오는 패킷 버퍼. 버스트 수신 시 드롭 방지 |

### 어태치먼트 생성

```swift
func attachments() -> [VZNetworkDeviceAttachment] {
  let fh = FileHandle.init(fileDescriptor: vmFD)
  return [VZFileHandleNetworkDeviceAttachment(fileHandle: fh)]
}
```

`VZFileHandleNetworkDeviceAttachment`는 파일 핸들을 통해 이더넷 프레임을 주고받는
저수준 네트워크 어태치먼트다. Softnet은 이를 활용하여 모든 패킷을 softnet 프로세스를
통해 중계한다.

### 프로세스 라이프사이클 관리

```swift
func run(_ sema: AsyncSemaphore) throws {
  try process.run()

  monitorTask = Task {
    // Softnet 종료 대기
    process.waitUntilExit()

    // 호출자에게 Softnet 종료 알림
    sema.signal()

    // 자체 상태 업데이트
    monitorTaskFinished.store(true, ordering: .sequentiallyConsistent)
  }
}

func stop() async throws {
  if monitorTaskFinished.load(ordering: .sequentiallyConsistent) {
    // 이미 종료된 경우: 비정상 종료
    _ = try await monitorTask?.value
    throw SoftnetError.RuntimeFailed(why: "Softnet process terminated prematurely")
  } else {
    // 정상 종료: SIGINT 전송
    process.interrupt()
    _ = try await monitorTask?.value
  }
}
```

```
Softnet 프로세스 라이프사이클:

  run() 호출
    │
    ├── process.run() → softnet 프로세스 시작
    │
    ├── monitorTask 생성
    │     └── process.waitUntilExit() 대기
    │
    │   [VM 실행 중...]
    │
    ├── 정상 종료 경로:
    │     stop() 호출
    │       └── process.interrupt() → SIGINT 전송
    │             └── softnet 프로세스 종료
    │                   └── monitorTask 완료
    │
    └── 비정상 종료 경로:
          softnet 프로세스가 자체 종료
            └── monitorTask 감지
                  └── sema.signal() → VM 실행 루프 중단
                        └── stop() 호출 시 RuntimeFailed 에러
```

**왜 `ManagedAtomic<Bool>`인가?** `stop()`이 호출될 때 softnet 프로세스가 이미 종료됐는지
확인해야 한다. 모니터 태스크와 `stop()` 호출이 서로 다른 스레드에서 실행될 수 있으므로,
원자적 불리언으로 상태를 안전하게 공유한다.

### SUID 비트 설정

Softnet은 macOS의 `vmnet.framework`에 접근하기 위해 root 권한이 필요하다. 이를 위해
SUID 비트를 설정한다:

```swift
static func configureSUIDBitIfNeeded() throws {
  // 실제 바이너리 경로 해제 (Homebrew 심볼릭 링크 대응)
  let softnetExecutablePath = try Softnet.softnetExecutableURL()
    .resolvingSymlinksInPath().path

  // 1. SUID 비트가 이미 설정되어 있는지 확인
  let info = try FileManager.default.attributesOfItem(atPath: softnetExecutablePath)
    as NSDictionary
  if info.fileOwnerAccountID() == 0 && (info.filePosixPermissions() & Int(S_ISUID)) != 0 {
    return
  }

  // 2. 비밀번호 없는 Sudo가 설정되어 있는지 확인
  var process = Process()
  process.executableURL = sudoExecutableURL
  process.arguments = ["--non-interactive", "softnet", "--help"]
  try process.run()
  process.waitUntilExit()
  if process.terminationStatus == 0 {
    return
  }

  // 3. 사용자에게 비밀번호 요청하여 SUID 설정
  fputs("Softnet requires a Sudo password to set the SUID bit...\n", stderr)

  process = try Process.run(sudoExecutableURL, arguments: [
    "sh", "-c",
    "chown root \(softnetExecutablePath) && chmod u+s \(softnetExecutablePath)",
  ])

  // sudo가 TTY 입력을 받을 수 있도록 포그라운드 전환
  if tcsetpgrp(STDIN_FILENO, process.processIdentifier) == -1 {
    let details = Errno(rawValue: CInt(errno))
    throw RuntimeError.SoftnetFailed("tcsetpgrp(2) failed: \(details)")
  }

  process.waitUntilExit()
  // ...
}
```

**왜 SUID인가?** macOS의 `vmnet.framework`는 네트워크 브리지를 생성하고 관리하기 위해
root 권한을 요구한다. softnet 바이너리에 SUID 비트를 설정하면, 일반 사용자가 실행해도
root 권한으로 vmnet에 접근할 수 있다. 이는 Tart 자체를 root로 실행하는 것보다 훨씬 안전하다.

**심볼릭 링크 해제(resolvingSymlinksInPath)를 하는 이유:** Homebrew로 설치하면
`/opt/homebrew/bin/softnet`이 실제로는 `/opt/homebrew/Cellar/softnet/0.6.2/bin/softnet`에
대한 심볼릭 링크다. SUID 비트는 심볼릭 링크가 아닌 실제 바이너리에 설정해야 한다.

### SUID 설정 흐름

```
configureSUIDBitIfNeeded() 호출
  │
  ├── 1. SUID 이미 설정됨? ──YES──► 리턴
  │      (owner==root && S_ISUID 비트 확인)
  │
  ├── 2. 비밀번호 없는 sudo 가능? ──YES──► 리턴
  │      (sudo --non-interactive softnet --help 성공)
  │
  └── 3. 사용자에게 sudo 비밀번호 요청
        │
        ├── tcsetpgrp()로 sudo 프로세스를 포그라운드로
        │
        ├── sudo sh -c "chown root ... && chmod u+s ..."
        │
        └── 성공/실패 확인
```

**`tcsetpgrp()` 호출이 필요한 이유:** sudo가 사용자 비밀번호를 입력받으려면 터미널의
포그라운드 프로세스 그룹이어야 한다. 그렇지 않으면 `SIGTTIN` 시그널을 받아 중지된다.
`tcsetpgrp(STDIN_FILENO, process.processIdentifier)`로 sudo 프로세스를 포그라운드로
전환한다.

## 6. Run 커맨드에서 네트워크 구성 흐름

소스 파일: `Sources/tart/Commands/Run.swift`

### 네트워크 옵션

Run 커맨드는 다양한 네트워크 관련 옵션을 제공한다:

| 옵션 | 타입 | 설명 |
|------|------|------|
| `--net-bridged` | `[String]` | 브리지할 인터페이스 (복수 지정 가능) |
| `--net-softnet` | `Bool` | Softnet 격리 네트워크 활성화 |
| `--net-softnet-allow` | `String?` | Softnet에서 허용할 CIDR |
| `--net-softnet-block` | `String?` | Softnet에서 차단할 CIDR |
| `--net-softnet-expose` | `String?` | Softnet 포트 포워딩 |
| `--net-host` | `Bool` | 호스트 전용 네트워크 |
| (없음) | - | 기본 Shared(NAT) |

### 상호 배타성 검증

```swift
mutating func validate() throws {
  // Softnet 관련 옵션이 있으면 자동으로 --net-softnet 활성화
  if netSoftnetAllow != nil || netSoftnetBlock != nil || netSoftnetExpose != nil {
    netSoftnet = true
  }

  // 네트워크 모드는 하나만 선택 가능
  var netFlags = 0
  if netBridged.count > 0 { netFlags += 1 }
  if netSoftnet { netFlags += 1 }
  if netHost { netFlags += 1 }

  if netFlags > 1 {
    throw ValidationError(
      "--net-bridged, --net-softnet and --net-host are mutually exclusive")
  }
}
```

**왜 상호 배타적인가?** 각 네트워크 모드는 VM에 서로 다른 유형의 네트워크 어태치먼트를
설정한다. 하나의 VM에 NAT와 브리지를 동시에 사용하면 라우팅 충돌과 예측 불가능한
네트워크 동작이 발생한다. 단, `--net-bridged`는 여러 인터페이스를 지원하여 단일 VM에
복수의 브리지 연결은 가능하다.

### userSpecifiedNetwork 메서드

```swift
func userSpecifiedNetwork(vmDir: VMDirectory) throws -> Network? {
  var softnetExtraArguments: [String] = []

  if let netSoftnetAllow = netSoftnetAllow {
    softnetExtraArguments += ["--allow", netSoftnetAllow]
  }
  if let netSoftnetBlock = netSoftnetBlock {
    softnetExtraArguments += ["--block", netSoftnetBlock]
  }
  if let netSoftnetExpose = netSoftnetExpose {
    softnetExtraArguments += ["--expose", netSoftnetExpose]
  }

  if netSoftnet {
    let config = try VMConfig.init(fromURL: vmDir.configURL)
    return try Softnet(vmMACAddress: config.macAddress.string,
                       extraArguments: softnetExtraArguments)
  }

  if netHost {
    let config = try VMConfig.init(fromURL: vmDir.configURL)
    return try Softnet(vmMACAddress: config.macAddress.string,
                       extraArguments: ["--vm-net-type", "host"] + softnetExtraArguments)
  }

  if netBridged.count > 0 {
    return NetworkBridged(interfaces: try netBridged.map { try findBridgedInterface($0) })
  }

  return nil  // → 호출측에서 NetworkShared()로 폴백
}
```

```
네트워크 선택 흐름도:

  userSpecifiedNetwork() 호출
    │
    ├── --net-softnet? ──YES──► Softnet(vmMACAddress, extraArgs)
    │
    ├── --net-host? ──YES──► Softnet(vmMACAddress,
    │                               ["--vm-net-type", "host"] + extraArgs)
    │
    ├── --net-bridged? ──YES──► NetworkBridged(interfaces)
    │
    └── 없음 ──► nil
                  │
                  └── 호출측: ?? NetworkShared()
```

**왜 `--net-host`도 Softnet을 사용하는가?** `--net-host` 모드는 VM이 호스트 머신에만
접근 가능하고 외부 네트워크에는 접근 불가능한 제한된 네트워크다. 이 제한을 softnet의
패킷 필터링으로 구현한다. `--vm-net-type host` 인자를 softnet에 전달하면, softnet이
호스트 전용 네트워크 규칙을 적용한다.

### SUID 설정 타이밍

```swift
// Sources/tart/Commands/Run.swift, run() 메서드 내
if (netSoftnet || netHost) && isInteractiveSession() {
  try Softnet.configureSUIDBitIfNeeded()
}
```

대화형 세션(터미널에서 직접 실행)에서만 SUID 설정을 시도한다. CI/CD 파이프라인
(비대화형)에서는 사용자 입력을 받을 수 없으므로 건너뛴다. CI/CD 환경에서는 사전에
softnet을 설정해 두어야 한다.

### MAC 주소 충돌 방지

Run 커맨드 실행 시, 같은 MAC 주소를 가진 다른 VM이 이미 실행 중인지 확인한다:

```swift
// Sources/tart/Commands/Run.swift
let storageLock = try FileLock(lockURL: Config().tartHomeDir)
try storageLock.lock()

let hasRunningMACCollision = try localStorage.list().contains {
  try $1.running() && $1.macAddress() == vmDir.macAddress() && $1.name != vmDir.name
}
if hasRunningMACCollision {
  print("There is already a running VM with the same MAC address!")
  print("Resetting VM to assign a new MAC address...")
  try vmDir.regenerateMACAddress()
}
```

**왜 전역 잠금이 필요한가?** MAC 충돌 검사와 재생성 사이에 다른 VM이 시작될 수 있다.
전역 잠금으로 이 경쟁 조건을 방지한다.

## 7. Softnet의 패킷 필터링 규칙

Softnet이 적용하는 기본 규칙:

```
┌──────────────────────────────────────────────────────┐
│              Softnet 패킷 필터링 규칙                   │
├──────────────────────────────────────────────────────┤
│                                                       │
│  [송신 규칙 - VM → 외부]                                │
│  1. 소스 MAC = VM의 MAC 주소여야 함 (스푸핑 방지)        │
│  2. 소스 IP = DHCP가 할당한 IP여야 함                    │
│  3. 목적지 IP는 글로벌 라우팅 가능 IPv4만 허용            │
│  4. 또는 vmnet 브리지 게이트웨이 IP 허용                 │
│                                                       │
│  [수신 규칙 - 외부 → VM]                                │
│  5. 모든 수신 트래픽 허용                                │
│                                                       │
│  [추가 옵션]                                            │
│  --allow CIDR: 지정 CIDR로의 트래픽 추가 허용            │
│  --block CIDR: 지정 CIDR로의 트래픽 차단                 │
│  --expose PORT: 포트 포워딩                             │
│  --vm-net-type host: 호스트 전용 네트워크                │
│                                                       │
│  [CIDR 충돌 시] longest prefix match 우선               │
│                같은 prefix면 block 우선                  │
│                                                       │
└──────────────────────────────────────────────────────┘
```

### 운영 시나리오별 네트워크 구성

**시나리오 1: CI/CD에서 격리된 VM 실행**

```bash
tart run my-vm --net-softnet
```
기본 Softnet 규칙 적용. VM은 인터넷에 접근 가능하지만, 같은 호스트의 다른 VM과는 격리됨.
DHCP 리스 시간이 600초로 단축되어 주소 고갈 방지.

**시나리오 2: 로컬 네트워크 접근 허용**

```bash
tart run my-vm --net-softnet-allow=192.168.0.0/24
```
기본 규칙에 추가로 192.168.0.0/24 대역(로컬 네트워크)으로의 트래픽 허용.
`--net-softnet-allow`를 지정하면 자동으로 `--net-softnet`이 활성화됨.

**시나리오 3: 완전 격리 후 선택적 허용**

```bash
tart run my-vm \
  --net-softnet-block=0.0.0.0/0 \
  --net-softnet-allow=10.0.0.1/32
```
모든 외부 트래픽 차단 후, 10.0.0.1에만 접근 허용.

**시나리오 4: 포트 포워딩**

```bash
tart run my-vm --net-softnet-expose=2222:22,8080:80
```
호스트의 2222 포트를 VM의 22 포트로, 8080을 80으로 포워딩.
호스트의 egress 인터페이스에서 수신하며, 외부 네트워크에서 접근 가능.

**시나리오 5: 호스트 전용**

```bash
tart run my-vm --net-host
```
VM은 호스트에만 접근 가능. 인터넷 접근 불가. 내부적으로
`Softnet(vmMACAddress, ["--vm-net-type", "host"])`로 구현.

## 8. IP 주소 조회 (tart ip)

소스 파일: `Sources/tart/Commands/IP.swift`

VM의 IP를 조회하는 3가지 전략:

```swift
enum IPResolutionStrategy: String, ExpressibleByArgument, CaseIterable {
  case dhcp, arp, agent
}
```

```swift
static public func resolveIP(_ vmMACAddress: MACAddress,
                              resolutionStrategy: IPResolutionStrategy = .dhcp,
                              secondsToWait: UInt16 = 0,
                              controlSocketURL: URL? = nil) async throws -> IPv4Address? {
  let waitUntil = Calendar.current.date(
    byAdding: .second, value: Int(secondsToWait), to: Date.now)!

  repeat {
    switch resolutionStrategy {
    case .arp:
      if let ip = try ARPCache().ResolveMACAddress(macAddress: vmMACAddress) {
        return ip
      }
    case .dhcp:
      if let leases = try Leases(),
         let ip = leases.ResolveMACAddress(macAddress: vmMACAddress) {
        return ip
      }
    case .agent:
      guard let controlSocketURL = controlSocketURL else {
        throw RuntimeError.Generic("...")
      }
      if let ip = try await AgentResolver.ResolveIP(
        controlSocketURL.relativePath) {
        return ip
      }
    }
    try await Task.sleep(nanoseconds: 1_000_000_000)
  } while Date.now < waitUntil

  return nil
}
```

| 전략 | 동작 방식 | 적용 대상 | 장점 | 단점 |
|------|----------|---------|------|------|
| `dhcp` (기본) | DHCP 리스 파일 파싱 | Shared(NAT) | 빠르고 안정적 | 브리지/Softnet에서 불가 |
| `arp` | ARP 테이블 파싱 | Bridged | 브리지 모드 지원 | VM의 네트워크 활동 필요 |
| `agent` | Guest Agent gRPC | 모든 모드 | 가장 안정적 | Guest Agent 설치 필요 |

**`--wait` 옵션:** VM이 부팅 중일 때 IP가 아직 할당되지 않았을 수 있다. `--wait=30`을
지정하면 최대 30초까지 1초 간격으로 재시도한다.

### 에러 메시지 분기

```swift
func run() async throws {
  // ...
  guard let ip = try await IP.resolveIP(vmMACAddress, ...) else {
    var message = "no IP address found"

    if try !vmDir.running() {
      message += ", is your VM running?"
    }

    if (resolver == .agent) {
      message += " (also make sure that Guest agent for Tart is running inside of a VM)"
    } else if (vmConfig.os == .linux && resolver == .arp) {
      message += " (not all Linux distributions are compatible with the ARP resolver)"
    }

    throw RuntimeError.NoIPAddressFound(message)
  }

  print(ip)
}
```

IP 조회 실패 시 사용자에게 도움이 되는 힌트를 제공한다: VM이 실행 중인지, Guest Agent가
설치되어 있는지, Linux ARP 호환성 문제인지 등.

## 9. 3가지 모드 비교 요약

```
┌──────────────┬──────────────┬──────────────┬──────────────┐
│   특성        │   Shared     │   Bridged    │   Softnet    │
│              │   (NAT)      │              │              │
├──────────────┼──────────────┼──────────────┼──────────────┤
│ 옵션          │ (기본)       │ --net-bridged│ --net-softnet│
│ 구현 클래스    │NetworkShared │NetworkBridged│ Softnet      │
│ VZ 어태치먼트  │ VZNATNetwork │ VZBridgedNet │ VZFileHandle │
│ 권한 필요      │ 없음         │ 없음          │ SUID root    │
│ 외부 프로세스  │ 없음         │ 없음          │ softnet      │
│ IP 할당       │ macOS DHCP   │ LAN DHCP     │ macOS DHCP   │
│ 인터넷 접근    │ NAT 통해     │ 직접          │ 필터링 후     │
│ VM 간 격리    │ 없음         │ 없음          │ 있음          │
│ IP 조회       │ dhcp         │ arp          │ agent        │
│ 적합한 환경    │ 개발/테스트   │ 네트워크 노출  │ CI/CD 프로덕션│
│ DHCP 리스     │ 86,400초     │ LAN 설정 따름  │ 600초        │
│ 다중 NIC     │ 불가          │ 가능          │ 불가          │
└──────────────┴──────────────┴──────────────┴──────────────┘
```

## 10. 에러 처리

### SoftnetError

```swift
enum SoftnetError: Error {
  case InitializationFailed(why: String)
  case RuntimeFailed(why: String)
}
```

| 에러 | 발생 조건 | 의미 |
|------|---------|------|
| `InitializationFailed` | socketpair 실패, softnet 바이너리 미발견, 소켓 버퍼 설정 실패 | 초기화 단계 실패 |
| `RuntimeFailed` | softnet 프로세스가 VM보다 먼저 종료 | 런타임 비정상 종료 |
| `RuntimeError.SoftnetFailed` | SUID 설정 실패, tcsetpgrp 실패 | 권한 설정 실패 |

### 실행 모드별 에러 복구

```
softnet 프로세스 비정상 종료 시:

  monitorTask 감지
    ↓
  sema.signal()
    ↓
  VM 실행 루프 중단
    ↓
  stop() 호출
    ↓
  monitorTaskFinished == true 확인
    ↓
  SoftnetError.RuntimeFailed 발생
    ↓
  VM 종료 + 에러 출력
```

## 11. 설계 결정 요약

| 설계 결정 | 선택 | 이유 |
|----------|------|------|
| 프로토콜 추상화 | `Network` 프로토콜 | 모드 독립적인 VM 초기화, 확장성 |
| 기본 모드 | Shared(NAT) | 무설정, 무권한, 대부분 충분 |
| 격리 구현 | 외부 softnet 프로세스 | SUID 최소 범위, 유저스페이스 필터링 |
| 프로세스 간 통신 | socketpair(AF_UNIX, SOCK_DGRAM) | 이더넷 프레임 경계 보존, 저지연 |
| 프로세스 모니터링 | AsyncSemaphore + Atomic | 비동기 환경에서 안전한 상태 공유 |
| SUID 설정 | 대화형에서만 | CI/CD에서는 사전 설정 필요 |
| 소켓 버퍼 | RCVBUF=4MB, SNDBUF=1MB | Apple 공식 권장 비율 |
| 상호 배타성 | 네트워크 모드 하나만 | 라우팅 충돌 방지 |
| IP 조회 | 3가지 전략 | 네트워크 모드별 최적 방법 제공 |
| 심볼릭 링크 해제 | resolvingSymlinksInPath | Homebrew 링크 우회, 실제 바이너리에 SUID |

## 12. 소스 파일 참조

| 파일 | 경로 | 역할 |
|------|------|------|
| Network.swift | `Sources/tart/Network/Network.swift` | Network 프로토콜 정의 |
| NetworkShared.swift | `Sources/tart/Network/NetworkShared.swift` | NAT 네트워크 구현 |
| NetworkBridged.swift | `Sources/tart/Network/NetworkBridged.swift` | 브리지 네트워크 구현 |
| Softnet.swift | `Sources/tart/Network/Softnet.swift` | Softnet 격리 네트워크 구현 |
| Run.swift | `Sources/tart/Commands/Run.swift` | 네트워크 옵션 파싱, 선택 로직 |
| IP.swift | `Sources/tart/Commands/IP.swift` | IP 조회 (dhcp/arp/agent) |
