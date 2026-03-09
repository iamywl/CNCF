# 19. Shell Completions, VNC 원격 접근, 유틸리티 심화

## 개요

Tart는 CLI 도구로서 사용자 경험을 향상시키기 위한 여러 보조 서브시스템을 포함한다.
이 문서에서는 **Shell Completions**(셸 자동완성), **VNC 원격 접근**(ScreenSharing/FullFledged),
**Serial 콘솔**(PTY), **패스프레이즈 생성**, **터미널 제어**(Term), **유틸리티 함수**를 다룬다.

이들은 P2-선택 기능이지만, Tart를 CI/CD 환경과 원격 디버깅에서 활용할 때 핵심적인 역할을 한다.

이 문서에서 다루는 항목:

1. Shell Completions -- Zsh 자동완성 커스텀 구현
2. VNC 프로토콜 추상화 -- Protocol 기반 이중 구현
3. ScreenSharingVNC -- macOS 내장 화면 공유
4. FullFledgedVNC -- Virtualization.Framework VNC 서버
5. Serial 콘솔 -- PTY 기반 직렬 콘솔
6. 터미널 제어 (Term) -- Raw 모드와 크기 감지
7. PassphraseGenerator -- BIP-39 기반 암호문 생성
8. Utils -- 경로 해석과 안전한 컬렉션 접근
9. Run.swift에서의 통합 -- VNC/Serial/Terminal 조합
10. 설계 철학: 왜 이렇게 만들었는가

---

## 1. Shell Completions

### 1.1 소스 위치

```
파일: Sources/tart/ShellCompletions/ShellCompletions.swift
```

Tart의 셸 자동완성은 `swift-argument-parser` 프레임워크의 `CompletionKind.custom` 기능과
Tart 자체 VM 목록 함수를 결합하여 구현된다.

### 1.2 핵심 함수 3개

Tart는 세 가지 자동완성 함수를 제공한다:

```swift
// ShellCompletions.swift
func completeMachines(...)        -> [String]  // 모든 VM (Local + OCI)
func completeLocalMachines(...)   -> [String]  // 로컬 VM만
func completeRunningMachines(...) -> [String]  // 실행 중인 VM만
```

각 함수의 시그니처:

```swift
func completeMachines(
  _ arguments: [String],        // 현재까지 입력된 인자
  _ argumentIdx: Int,           // 완성할 인자의 인덱스
  _ argumentPrefix: String      // 현재 입력된 접두사
) -> [String]
```

### 1.3 completeMachines -- 전체 VM 목록

```swift
// ShellCompletions.swift 8~16행
func completeMachines(_ arguments: [String], _ argumentIdx: Int,
                      _ argumentPrefix: String) -> [String] {
  let localVMs = (try? VMStorageLocal().list().map { name, _ in
    normalizeName(name)
  }) ?? []
  let ociVMs = (try? VMStorageOCI().list().map { name, _, _ in
    normalizeName(name)
  }) ?? []
  return (localVMs + ociVMs)
}
```

두 가지 스토리지에서 VM 목록을 가져와 합친다:
- **VMStorageLocal**: `~/.tart/vms/` 디렉토리의 로컬 VM
- **VMStorageOCI**: `~/.tart/cache/OCIs/` 디렉토리의 OCI 캐시 VM

`list()` 반환 타입이 다르다:
- Local: `(name: String, vmDir: VMDirectory)` 튜플
- OCI: `(name: String, vmDir: VMDirectory, tag: String)` 튜플 (태그 포함)

### 1.4 completeLocalMachines -- 로컬 VM만

```swift
// ShellCompletions.swift 18~21행
func completeLocalMachines(_ arguments: [String], _ argumentIdx: Int,
                           _ argumentPrefix: String) -> [String] {
  let localVMs = (try? VMStorageLocal().list()) ?? []
  return localVMs.map { name, _ in normalizeName(name) }
}
```

`tart run`, `tart set`, `tart delete` 등 로컬 VM을 대상으로 하는 명령에서 사용한다.

### 1.5 completeRunningMachines -- 실행 중 VM만

```swift
// ShellCompletions.swift 23~28행
func completeRunningMachines(_ arguments: [String], _ argumentIdx: Int,
                             _ argumentPrefix: String) -> [String] {
  let localVMs = (try? VMStorageLocal().list()) ?? []
  return localVMs
    .filter { _, vmDir in (try? vmDir.state() == .Running) ?? false }
    .map { name, _ in normalizeName(name) }
}
```

`vmDir.state()`로 VM 실행 상태를 확인한다. `.Running` 상태인 VM만 반환한다.
`tart stop`, `tart suspend`, `tart ip` 등 실행 중인 VM을 대상으로 하는 명령에 사용한다.

### 1.6 normalizeName -- Zsh 특수문자 이스케이프

```swift
// ShellCompletions.swift 3~6행
fileprivate func normalizeName(_ name: String) -> String {
  // Colons are misinterpreted by Zsh completion
  return name.replacingOccurrences(of: ":", with: "\\:")
}
```

**왜 콜론을 이스케이프하는가?**

Zsh의 자동완성 시스템에서 콜론(`:`)은 특수 의미를 가진다:
- `completion:description` 형태로 완성 항목과 설명을 구분하는 구분자
- OCI 이미지 이름에 콜론이 포함됨: `ghcr.io/cirruslabs/macos-sonoma-base:latest`
- 이스케이프 없이는 `ghcr.io/cirruslabs/macos-sonoma-base` 뒤의 `:latest`가 설명으로 해석됨

### 1.7 커맨드에서의 사용

```swift
// Commands/Run.swift 68행
@Argument(help: "VM name", completion: .custom(completeLocalMachines))
var name: String
```

`swift-argument-parser`의 `completion:` 매개변수에 커스텀 함수를 전달한다.
셸이 `tart run <TAB>`을 입력하면 이 함수가 호출되어 후보 목록을 반환한다.

### 1.8 자동완성 흐름 다이어그램

```
사용자: tart run <TAB>
    │
    ▼
Zsh Completion 엔진
    │
    ▼
swift-argument-parser
    │  completion: .custom(completeLocalMachines)
    ▼
completeLocalMachines()
    │
    ├── VMStorageLocal().list()
    │       ├── ~/.tart/vms/ 디렉토리 스캔
    │       └── [(name, vmDir), ...] 반환
    │
    ├── .map { normalizeName($0.name) }
    │       └── 콜론 이스케이프
    │
    └── ["vm-1", "ghcr.io/cirruslabs/macos-sonoma-base\\:latest", ...]
         │
         ▼
    Zsh: 후보 목록 표시
```

---

## 2. VNC 프로토콜 추상화

### 2.1 VNC Protocol 정의

```
파일: Sources/tart/VNC/VNC.swift
```

```swift
// VNC.swift 전체
protocol VNC {
  func waitForURL(netBridged: Bool) async throws -> URL
  func stop() throws
}
```

이 프로토콜은 두 가지 VNC 구현의 공통 인터페이스를 정의한다:

| 메서드 | 설명 |
|--------|------|
| `waitForURL(netBridged:)` | VNC 서버가 준비될 때까지 대기, URL 반환 |
| `stop()` | VNC 서버 정지 |

`async throws`를 사용하여 비동기적으로 VNC URL을 해석한다.
IP 주소 해석이 네트워크 환경에 따라 시간이 걸릴 수 있기 때문이다.

### 2.2 두 가지 VNC 구현

```
Sources/tart/VNC/
├── VNC.swift                 # Protocol 정의
├── ScreenSharingVNC.swift    # macOS 내장 화면 공유
└── FullFledgedVNC.swift      # VZ 프레임워크 VNC 서버
```

| 항목 | ScreenSharingVNC | FullFledgedVNC |
|------|------------------|----------------|
| 연결 대상 | VM 내부 VNC 서버 | 호스트의 VZ VNC 서버 |
| 플래그 | `--vnc` | `--vnc-experimental` |
| IP 해석 | VM의 MAC → IP | 127.0.0.1 (로컬) |
| 인증 | VM 내부 설정 | 자동 생성 패스프레이즈 |
| macOS 설치 | 불가 | 가능 |
| 복구 모드 | 불가 | 가능 |
| 안정성 | 안정 | 실험적 |

### 2.3 왜 두 가지 구현이 필요한가?

**ScreenSharingVNC**는 VM 내부에서 동작하는 화면 공유 서비스에 연결한다.
VM의 게스트 OS가 완전히 부팅되고 화면 공유 서비스가 활성화된 후에만 사용할 수 있다.

**FullFledgedVNC**는 `Virtualization.Framework`의 `_VZVNCServer` (비공개 API)를 사용하여
하이퍼바이저 수준에서 VNC 서버를 실행한다. 게스트 OS 상태와 무관하게 동작하므로:
- macOS 설치 과정을 원격으로 관찰 가능
- 복구 모드에서도 사용 가능
- 게스트 OS에 화면 공유 서비스가 없어도 동작

단, 비공개 API(`_VZVNCServer`)를 사용하므로 "experimental" 플래그로 분리한다.

---

## 3. ScreenSharingVNC

### 3.1 소스 코드

```
파일: Sources/tart/VNC/ScreenSharingVNC.swift
```

```swift
// ScreenSharingVNC.swift 전체
class ScreenSharingVNC: VNC {
  let vmConfig: VMConfig

  init(vmConfig: VMConfig) {
    self.vmConfig = vmConfig
  }

  func waitForURL(netBridged: Bool) async throws -> URL {
    let vmMACAddress = MACAddress(fromString: vmConfig.macAddress.string)!
    let ip = try await IP.resolveIP(
      vmMACAddress,
      resolutionStrategy: netBridged ? .arp : .dhcp,
      secondsToWait: 60
    )

    if let ip = ip {
      return URL(string: "vnc://\(ip)")!
    }

    throw IPNotFound()
  }

  func stop() throws {
    // nothing to do
  }
}
```

### 3.2 IP 해석 전략

`waitForURL`은 VM의 MAC 주소로부터 IP를 해석한다.
네트워크 모드에 따라 두 가지 전략을 사용한다:

```
┌─────────────────────────────────────────────────┐
│          IP 해석 전략 결정                        │
│                                                   │
│  netBridged == true  → .arp (ARP 테이블 조회)     │
│  netBridged == false → .dhcp (DHCP 리스 조회)     │
│                                                   │
│  최대 대기: 60초                                  │
│  실패 시: IPNotFound 에러                          │
└─────────────────────────────────────────────────┘
```

**ARP 전략** (Bridged 네트워크):
- VM이 호스트와 같은 L2 네트워크에 있음
- `arp -a` 또는 ARP 캐시에서 MAC → IP 매핑 조회
- Bridged 모드에서는 DHCP 서버가 호스트가 아닌 외부에 있으므로 ARP가 더 적합

**DHCP 전략** (Shared 네트워크):
- `vmnet` 프레임워크의 NAT 네트워크
- 호스트의 DHCP 리스 파일(`/var/db/dhcpd_leases`)에서 MAC → IP 조회
- Shared 모드에서 호스트가 DHCP 서버 역할

### 3.3 URL 형식

```
vnc://192.168.64.5
```

포트를 지정하지 않으므로 기본 VNC 포트(5900)를 사용한다.
macOS의 "화면 공유" 앱이 이 URL을 처리한다.

### 3.4 stop()이 비어있는 이유

ScreenSharingVNC는 VM 내부 서비스에 연결할 뿐, 호스트에서 서버를 생성하지 않는다.
따라서 정리할 리소스가 없다. VM이 종료되면 자동으로 연결이 끊어진다.

---

## 4. FullFledgedVNC

### 4.1 소스 코드

```
파일: Sources/tart/VNC/FullFledgedVNC.swift
```

```swift
// FullFledgedVNC.swift 전체
class FullFledgedVNC: VNC {
  let password: String
  private let vnc: Dynamic

  init(virtualMachine: VZVirtualMachine) {
    password = Array(PassphraseGenerator().prefix(4)).joined(separator: "-")
    let securityConfiguration =
      Dynamic._VZVNCAuthenticationSecurityConfiguration(password: password)
    vnc = Dynamic._VZVNCServer(
      port: 0,
      queue: DispatchQueue.global(),
      securityConfiguration: securityConfiguration
    )
    vnc.virtualMachine = virtualMachine
    vnc.start()
  }

  func waitForURL(netBridged: Bool) async throws -> URL {
    while true {
      if let port = vnc.port.asUInt16, port != 0 {
        return URL(string: "vnc://:\(password)@127.0.0.1:\(port)")!
      }
      try await Task.sleep(nanoseconds: 50_000_000)  // 50ms
    }
  }

  func stop() throws {
    vnc.stop()
  }

  deinit {
    try? stop()
  }
}
```

### 4.2 Dynamic 라이브러리

`_VZVNCServer`는 Apple의 비공개 API이다.
Swift에서 비공개 API를 호출하려면 `@objc` 런타임 메시지 전송이 필요하다.
Tart는 [Dynamic](https://github.com/mhdhejazi/Dynamic) 라이브러리를 사용하여 이를 해결한다:

```swift
// Dynamic을 사용한 비공개 API 호출
let vnc = Dynamic._VZVNCServer(
  port: 0,                           // 임의 포트 할당
  queue: DispatchQueue.global(),      // 전역 디스패치 큐
  securityConfiguration: secConfig    // VNC 인증 설정
)
```

`Dynamic`은 Objective-C 런타임의 `objc_msgSend`를 래핑하여
컴파일 타임에 존재하지 않는 클래스와 메서드를 호출할 수 있게 한다.

### 4.3 패스프레이즈 기반 인증

```swift
password = Array(PassphraseGenerator().prefix(4)).joined(separator: "-")
// 결과 예: "abandon-ability-able-about"
```

VNC 서버 시작 시 4단어 패스프레이즈를 자동 생성하여 인증에 사용한다.
이 패스프레이즈는 콘솔에 출력되어 사용자가 VNC 클라이언트에서 입력한다.

**왜 패스프레이즈를 사용하는가?**
- FullFledgedVNC는 127.0.0.1에 바인딩하지만, 포트 포워딩으로 외부 노출 가능
- 무인 CI/CD 환경에서도 최소한의 인증이 필요
- 일회성 생성이므로 설정 파일이 불필요

### 4.4 포트 할당과 폴링

```swift
// port: 0 → OS가 임의 포트 할당
vnc = Dynamic._VZVNCServer(port: 0, ...)
```

포트 0을 지정하면 OS가 사용 가능한 포트를 자동 할당한다.
`start()` 호출 직후에는 포트가 아직 0일 수 있으므로, `waitForURL`에서 50ms 간격으로 폴링한다:

```swift
while true {
  if let port = vnc.port.asUInt16, port != 0 {
    return URL(string: "vnc://:\(password)@127.0.0.1:\(port)")!
  }
  try await Task.sleep(nanoseconds: 50_000_000)  // 50ms
}
```

### 4.5 URL 형식

```
vnc://:abandon-ability-able-about@127.0.0.1:54321
```

- 사용자명 없음 (`:` 앞이 비어있음)
- 패스프레이즈가 비밀번호 위치에
- 127.0.0.1 (로컬 연결)
- OS가 할당한 임의 포트

### 4.6 리소스 정리

```swift
func stop() throws {
  vnc.stop()
}

deinit {
  try? stop()
}
```

`FullFledgedVNC`는 호스트에서 VNC 서버를 실행하므로 명시적 정리가 필요하다.
`deinit`에서도 `stop()`을 호출하여 메모리 해제 시 서버가 확실히 종료되도록 한다.

---

## 5. Serial 콘솔

### 5.1 소스 위치

```
파일: Sources/tart/Serial.swift
```

### 5.2 PTY (Pseudo Terminal) 생성

```swift
// Serial.swift 전체
func createPTY() -> Int32 {
  var tty_fd: Int32 = -1
  var sfd: Int32 = -1
  var termios_ = termios()
  let tty_path = UnsafeMutablePointer<CChar>.allocate(capacity: 1024)

  var res = openpty(&tty_fd, &sfd, tty_path, nil, nil)
  if res < 0 {
    perror("openpty error")
    return -1
  }

  // close slave file descriptor
  close(sfd)

  res = fcntl(tty_fd, F_GETFL)
  if res < 0 {
    perror("fcntl F_GETFL error")
    return res
  }

  // set serial nonblocking
  res = fcntl(tty_fd, F_SETFL, res | O_NONBLOCK)
  if res < 0 {
    perror("fcntl F_SETFL O_NONBLOCK error")
    return res
  }

  // set baudrate to 115200
  tcgetattr(tty_fd, &termios_)
  cfsetispeed(&termios_, speed_t(B115200))
  cfsetospeed(&termios_, speed_t(B115200))
  if tcsetattr(tty_fd, TCSANOW, &termios_) != 0 {
    perror("tcsetattr error")
    return -1
  }

  print("Successfully open pty \(String(cString: tty_path))")

  tty_path.deallocate()
  return tty_fd
}
```

### 5.3 PTY 생성 과정 상세

PTY는 Master/Slave 쌍으로 구성된다:

```
┌─────────────┐     PTY Pair      ┌─────────────┐
│   Master     │◄────────────────►│   Slave      │
│  (tty_fd)    │   양방향 파이프   │  (sfd)       │
│              │                   │              │
│  Tart 호스트 │                   │  VM 시리얼   │
│  프로세스     │                   │  콘솔        │
└─────────────┘                   └─────────────┘
```

단계별 동작:

**1단계: openpty()**
```c
openpty(&tty_fd, &sfd, tty_path, nil, nil)
```
- `tty_fd`: Master 파일 디스크립터
- `sfd`: Slave 파일 디스크립터
- `tty_path`: Slave 경로 (예: `/dev/ttys004`)

**2단계: Slave 닫기**
```c
close(sfd)
```
Tart는 Master만 사용한다. Slave는 `Virtualization.Framework`가
`VZVirtioConsoleDeviceSerialPortConfiguration`을 통해 접근한다.

**3단계: Non-blocking 설정**
```c
fcntl(tty_fd, F_SETFL, res | O_NONBLOCK)
```
Non-blocking으로 설정하여 데이터가 없을 때 읽기가 블로킹되지 않도록 한다.
VM이 시리얼 콘솔에 데이터를 보내지 않는 동안에도 이벤트 루프가 차단되지 않는다.

**4단계: Baud Rate 설정**
```c
cfsetispeed(&termios_, speed_t(B115200))
cfsetospeed(&termios_, speed_t(B115200))
```
가상 시리얼이므로 물리적 baud rate는 의미가 없지만, PTY 드라이버의 프로토콜 호환을 위해 설정한다.
115200은 가상 시리얼의 표준 속도이다.

### 5.4 Run.swift에서의 Serial 통합

```swift
// Commands/Run.swift 77~85행
@Flag(help: "Open serial console in /dev/ttySXX")
var serial: Bool = false

@Option(help: ArgumentHelp(
  "Attach an externally created serial console",
  discussion: "Alternative to --serial flag for programmatic integrations."
), completion: .file())
var serialPath: String?
```

두 가지 모드:
- `--serial`: Tart가 PTY를 자동 생성
- `--serial-path /dev/ttys004`: 외부에서 만든 PTY 경로를 지정

```swift
// Commands/Run.swift 390~405행
var serialPorts: [VZSerialPortConfiguration] = []
if serial {
  let tty_fd = createPTY()        // PTY 자동 생성
  if tty_fd < 0 {
    throw RuntimeError.VMConfigurationError("Failed to create PTY")
  }
  let tty_read = FileHandle(fileDescriptor: tty_fd)
  let tty_write = FileHandle(fileDescriptor: tty_fd)
  serialPorts.append(createSerialPortConfiguration(tty_read, tty_write))
} else if serialPath != nil {
  let tty_read = FileHandle(forReadingAtPath: serialPath!)   // 외부 PTY 연결
  let tty_write = FileHandle(forWritingAtPath: serialPath!)
  if tty_read == nil || tty_write == nil {
    throw RuntimeError.VMConfigurationError("Failed to open PTY")
  }
  serialPorts.append(createSerialPortConfiguration(tty_read!, tty_write!))
}
```

### 5.5 VZVirtioConsoleDeviceSerialPortConfiguration

```swift
// Commands/Run.swift 608~615행
private func createSerialPortConfiguration(
  _ tty_read: FileHandle,
  _ tty_write: FileHandle
) -> VZVirtioConsoleDeviceSerialPortConfiguration {
  let serialPortConfiguration = VZVirtioConsoleDeviceSerialPortConfiguration()
  let serialPortAttachment = VZFileHandleSerialPortAttachment(
    fileHandleForReading: tty_read,
    fileHandleForWriting: tty_write
  )
  serialPortConfiguration.attachment = serialPortAttachment
  return serialPortConfiguration
}
```

`VZFileHandleSerialPortAttachment`가 FileHandle을 Virtualization.Framework의
시리얼 포트에 연결한다. 게스트 OS에서 `/dev/ttyS0`로 보이는 가상 시리얼 포트이다.

### 5.6 시리얼 콘솔 사용 시나리오

```
┌────────────────────────────────────────────────────┐
│  시리얼 콘솔 활용 시나리오                            │
│                                                      │
│  1. 리눅스 VM 부팅 디버깅                             │
│     tart run linux-vm --serial                       │
│     → /dev/ttys004 에서 커널 부트 로그 확인            │
│                                                      │
│  2. CI/CD 파이프라인 자동화                            │
│     mkfifo /tmp/serial-pipe                           │
│     tart run vm --serial-path /tmp/serial-pipe       │
│     → 스크립트에서 /tmp/serial-pipe로 명령 전송        │
│                                                      │
│  3. 네트워크 없는 환경                                │
│     → SSH 대신 시리얼로 VM 접근                       │
│     → IP 주소 할당 전에도 접근 가능                    │
└────────────────────────────────────────────────────┘
```

---

## 6. 터미널 제어 (Term)

### 6.1 소스 위치

```
파일: Sources/tart/Term.swift
```

### 6.2 Term 클래스 구조

```swift
// Term.swift 전체
struct State {
  fileprivate let termios: termios
}

class Term {
  static func IsTerminal() -> Bool { ... }
  static func MakeRaw() throws -> State { ... }
  static func Restore(_ state: State) throws { ... }
  static func GetSize() throws -> (width: UInt16, height: UInt16) { ... }
}
```

모든 메서드가 `static`이다. 터미널은 프로세스당 하나이므로 인스턴스가 불필요하다.

### 6.3 IsTerminal() -- 터미널 여부 감지

```swift
static func IsTerminal() -> Bool {
  var termios = termios()
  return tcgetattr(FileHandle.standardInput.fileDescriptor, &termios) != -1
}
```

`tcgetattr`이 성공하면 표준 입력이 터미널이다.
파이프(`echo "cmd" | tart exec ...`)나 리다이렉션에서는 실패한다.

**사용 이유**: Guest Agent의 `tart exec` 명령에서 터미널 여부에 따라 동작을 분기한다.
- 터미널: Raw 모드로 전환, 상호작용 가능
- 비터미널: Raw 모드 전환 생략, 스크립팅 호환

### 6.4 MakeRaw() -- Raw 모드 전환

```swift
static func MakeRaw() throws -> State {
  var termiosOrig = termios()

  var ret = tcgetattr(FileHandle.standardInput.fileDescriptor, &termiosOrig)
  if ret == -1 {
    let details = Errno(rawValue: CInt(errno))
    throw RuntimeError.TerminalOperationFailed(
      "failed to retrieve terminal parameters: \(details)"
    )
  }

  var termiosRaw = termiosOrig
  cfmakeraw(&termiosRaw)

  ret = tcsetattr(FileHandle.standardInput.fileDescriptor, TCSANOW, &termiosRaw)
  if ret == -1 {
    let details = Errno(rawValue: CInt(errno))
    throw RuntimeError.TerminalOperationFailed(
      "failed to set terminal parameters: \(details)"
    )
  }

  return State(termios: termiosOrig)
}
```

**Raw 모드란?**

일반 터미널("cooked 모드")은 입력을 줄 단위로 버퍼링하고 특수 키를 처리한다:
- Enter를 누를 때까지 입력을 모음
- Ctrl+C → SIGINT 신호
- Ctrl+D → EOF
- Backspace → 문자 삭제

Raw 모드에서는 이 모든 처리를 비활성화한다:
- 키 입력이 즉시 프로그램에 전달됨
- 특수 키가 원시 바이트로 전달됨
- VM의 시리얼 콘솔이나 Guest Agent에서 대화형 셸을 사용할 때 필요

`cfmakeraw()`는 다음 termios 플래그를 변경한다:

```
비활성화 플래그:
  ECHO     -- 입력 에코 끔
  ECHONL   -- 개행 에코 끔
  ICANON   -- 정규 모드(줄 버퍼링) 끔
  ISIG     -- INTR/QUIT/SUSP 시그널 끔
  IEXTEN   -- 확장 입력 처리 끔
  IXON     -- XON/XOFF 흐름 제어 끔
  ICRNL    -- CR→NL 변환 끔
  OPOST    -- 출력 후처리 끔
```

### 6.5 Restore() -- 원래 상태 복원

```swift
static func Restore(_ state: State) throws {
  var termios = state.termios
  let ret = tcsetattr(FileHandle.standardInput.fileDescriptor, TCSANOW, &termios)
  if ret == -1 {
    let details = Errno(rawValue: CInt(errno))
    throw RuntimeError.TerminalOperationFailed(
      "failed to set terminal parameters: \(details)"
    )
  }
}
```

`MakeRaw()`가 반환한 `State`에 원래 termios 설정이 저장되어 있다.
프로그램 종료 전에 반드시 `Restore`를 호출해야 터미널이 정상 상태로 돌아온다.

**왜 State 구조체를 사용하는가?**

`termios` 구조체를 직접 노출하면 사용자가 임의로 수정할 수 있다.
`fileprivate`로 감싸서 `MakeRaw()` → `Restore()` 쌍으로만 사용하도록 강제한다:

```swift
struct State {
  fileprivate let termios: termios  // 외부에서 직접 접근 불가
}
```

### 6.6 GetSize() -- 터미널 크기 조회

```swift
static func GetSize() throws -> (width: UInt16, height: UInt16) {
  var winsize = winsize()
  guard ioctl(STDOUT_FILENO, TIOCGWINSZ, &winsize) != -1 else {
    let details = Errno(rawValue: CInt(errno))
    throw RuntimeError.TerminalOperationFailed(
      "failed to get terminal size: \(details)"
    )
  }
  return (width: winsize.ws_col, height: winsize.ws_row)
}
```

`TIOCGWINSZ` ioctl로 터미널 창의 열(column) 수와 행(row) 수를 가져온다.
Guest Agent에서 원격 셸의 PTY 크기를 게스트에 전달할 때 사용한다.

---

## 7. PassphraseGenerator

### 7.1 소스 위치

```
파일: Sources/tart/Passphrase/PassphraseGenerator.swift
파일: Sources/tart/Passphrase/Words.swift
```

### 7.2 구현

```swift
// PassphraseGenerator.swift
struct PassphraseGenerator: Sequence {
  func makeIterator() -> PassphraseIterator {
    PassphraseIterator()
  }
}

struct PassphraseIterator: IteratorProtocol {
  mutating func next() -> String? {
    passphrases[Int(arc4random_uniform(UInt32(passphrases.count)))]
  }
}
```

Swift의 `Sequence` 프로토콜을 구현하여 무한 단어 스트림을 생성한다.

### 7.3 BIP-39 단어 목록

```swift
// Words.swift 2행
// https://github.com/bitcoin/bips/blob/master/bip-0039/english.txt
let passphrases = [
  "abandon",
  "ability",
  "able",
  ...
]
```

Bitcoin의 BIP-39 표준 영어 단어 목록(2048개)을 사용한다.
이 목록은:
- 각 단어가 4자 이상으로 입력이 편리
- 처음 4글자만으로 고유하게 식별 가능
- 발음과 철자가 명확하여 구두 전달에 적합

### 7.4 Sequence 패턴의 장점

`Sequence` 프로토콜 구현으로 Swift의 표준 컬렉션 연산을 모두 사용할 수 있다:

```swift
// 4개 단어 추출
Array(PassphraseGenerator().prefix(4))
// → ["abandon", "ability", "able", "about"]

// 하이픈으로 결합
Array(PassphraseGenerator().prefix(4)).joined(separator: "-")
// → "abandon-ability-able-about"
```

`next()`가 항상 `String?`을 반환하므로 무한 시퀀스이다.
`prefix(4)`로 필요한 만큼만 추출한다.

### 7.5 보안 강도

```
단어 수: 2048 = 2^11
4단어 조합: 2048^4 = 2^44 ≈ 17.6조 가지
```

VNC 접근용 일회성 패스프레이즈로는 충분한 강도이다.
`arc4random_uniform`은 암호학적으로 안전한 난수 생성기(CSPRNG)이다.

### 7.6 사용처

현재 Tart에서 패스프레이즈는 `FullFledgedVNC`에서만 사용한다:

```swift
// FullFledgedVNC.swift 10행
password = Array(PassphraseGenerator().prefix(4)).joined(separator: "-")
```

---

## 8. Utils

### 8.1 소스 위치

```
파일: Sources/tart/Utils.swift
```

### 8.2 안전한 컬렉션 접근

```swift
// Utils.swift 3~7행
extension Collection {
  subscript(safe index: Index) -> Element? {
    indices.contains(index) ? self[index] : nil
  }
}
```

Swift 배열은 범위 밖 인덱스에 접근하면 런타임 크래시가 발생한다.
이 확장은 `nil`을 반환하여 안전하게 처리한다:

```swift
let arr = [1, 2, 3]
arr[5]          // 💥 Fatal error: Index out of range
arr[safe: 5]    // nil
```

**사용 예시**: 명령줄 인자를 파싱할 때 인자가 부족한 경우를 안전하게 처리한다.

### 8.3 바이너리 경로 해석

```swift
// Utils.swift 9~24행
func resolveBinaryPath(_ name: String) -> URL? {
  guard let path = ProcessInfo.processInfo.environment["PATH"] else {
    return nil
  }

  for pathComponent in path.split(separator: ":") {
    let url = URL(fileURLWithPath: String(pathComponent))
      .appendingPathComponent(name, isDirectory: false)

    if FileManager.default.fileExists(atPath: url.path) {
      return url
    }
  }

  return nil
}
```

쉘의 `which` 명령과 동일한 기능이다. `PATH` 환경 변수를 순회하며 실행 파일을 찾는다.

**사용 이유**: Tart는 외부 도구를 호출하는 경우가 있다.
`softnet`(Softnet 네트워크 모드), `cloud-hypervisor`(Linux VM) 등의 바이너리 위치를 찾을 때 사용한다.

**왜 Foundation의 Process를 직접 사용하지 않는가?**

`Process()`의 `executableURL`은 전체 경로가 필요하다.
`PATH`에서 자동으로 찾아주지 않으므로 직접 해석해야 한다.

```
PATH 해석 흐름:
  PATH = "/usr/local/bin:/usr/bin:/bin"
     │
     ├── /usr/local/bin/softnet → 존재? → Yes → 반환
     ├── /usr/bin/softnet       → 존재? → No
     └── /bin/softnet           → 존재? → No
```

---

## 9. Run.swift에서의 통합

### 9.1 VNC 모드 선택

```swift
// Commands/Run.swift 109~117행
@Flag(help: "Use Screen Sharing VNC connection")
var vnc: Bool = false

@Flag(help: ArgumentHelp(
  "Use Virtualization.Framework's VNC server...",
  discussion: "... experimental and there may be bugs..."
))
var vncExperimental: Bool = false
```

두 플래그는 상호 배타적이다:

```swift
// Commands/Run.swift 289~290행
if vnc && vncExperimental {
  throw ValidationError("--vnc and --vnc-experimental are mutually exclusive")
}
```

### 9.2 VNC 인스턴스 생성

```swift
// Commands/Run.swift 428~433행
let vncImpl: VNC? = try {
  if vnc {
    return ScreenSharingVNC(vmConfig: vmConfig)
  } else if vncExperimental {
    return FullFledgedVNC(virtualMachine: vm!.virtualMachine)
  }
  return nil
}()
```

Protocol 기반 다형성으로 `VNC?` 타입 하나로 두 구현을 처리한다.

### 9.3 VNC URL 표시

```swift
// Commands/Run.swift 505~512행
if let vncImpl = vncImpl {
  let vncURL = try await vncImpl.waitForURL(netBridged: !netBridged.isEmpty)

  if noGraphics || useVNCWithoutGraphics {
    print("VNC server is running at \(vncURL)")
  } else {
    print("Opening \(vncURL)...")
    NSWorkspace.shared.open(vncURL)  // macOS 화면 공유 앱 자동 열기
  }
}
```

- `--no-graphics` 모드: URL만 출력 (CI/CD 환경, SSH 접속)
- GUI 모드: `NSWorkspace.shared.open()`으로 화면 공유 앱 자동 실행

### 9.4 VNC 정리

```swift
// Commands/Run.swift 524~525행
if let vncImpl = vncImpl {
  try vncImpl.stop()
}
```

VM 종료 시 VNC 서버도 정리한다.

### 9.5 Graphics + VNC 조합

```swift
// Commands/Run.swift 312행
if (noGraphics || vnc || vncExperimental) && captureSystemKeys {
  // 경고 또는 에러
}

// Commands/Run.swift 596~597행
let useVNCWithoutGraphics = (vnc || vncExperimental) && !graphics
if noGraphics || useVNCWithoutGraphics {
  // headless 실행
}
```

| `--no-graphics` | `--vnc` | 동작 |
|:---:|:---:|------|
| X | X | GUI 창 표시 |
| O | X | headless (VNC 없음) |
| X | O | GUI + VNC URL 출력, 화면 공유 앱 열기 |
| O | O | headless + VNC URL 출력 |

### 9.6 Serial + VNC + Graphics 전체 조합

```
┌─────────────────────────────────────────────────────────────┐
│  tart run vm [옵션]                                          │
│                                                               │
│  디스플레이:                                                  │
│    (기본)          → GUI 창                                   │
│    --no-graphics   → headless                                │
│    --vnc           → macOS 화면 공유로 연결                    │
│    --vnc-exp       → VZ VNC 서버 (실험적)                     │
│                                                               │
│  시리얼:                                                      │
│    --serial        → PTY 자동 생성, /dev/ttysXXX 출력         │
│    --serial-path   → 외부 PTY 연결                            │
│                                                               │
│  조합 예:                                                     │
│    tart run vm --no-graphics --serial                        │
│    → headless + 시리얼 콘솔만으로 접근                         │
│                                                               │
│    tart run vm --vnc-experimental --serial                   │
│    → VNC + 시리얼 동시 사용 (디버깅)                           │
└─────────────────────────────────────────────────────────────┘
```

---

## 10. 설계 철학: 왜 이렇게 만들었는가

### 10.1 Protocol 기반 VNC 추상화

두 VNC 구현은 완전히 다른 메커니즘을 사용하지만 동일한 인터페이스를 제공한다.
`Run.swift`는 구현 세부사항을 모른 채 `VNC?` 타입만 사용한다.

이 설계의 장점:
- **새로운 VNC 구현 추가가 용이**: 예를 들어 향후 Apple이 공개 VNC API를 제공하면 쉽게 추가
- **Run.swift의 복잡도를 억제**: VNC 로직이 각 구현 클래스에 캡슐화

### 10.2 PTY vs Named Pipe

시리얼 콘솔에 PTY를 사용하는 이유:
- PTY는 터미널 시맨틱을 제공 (창 크기 변경, 시그널 전달 등)
- Named Pipe는 단순 바이트 스트림만 제공
- VM 게스트의 `/dev/ttyS0`가 실제 시리얼 포트처럼 동작하려면 PTY가 필요

### 10.3 BIP-39 단어 목록 선택

VNC 패스프레이즈에 BIP-39 목록을 선택한 이유:
- 2048개 단어로 충분한 엔트로피
- 발음이 명확하여 구두 전달 가능 (CI 로그에서 읽어 입력)
- 검증된 표준 목록으로 단어 품질 보장
- Swift `Sequence` 패턴과 자연스럽게 결합

### 10.4 Zsh 특화 자동완성

Tart는 Zsh만 특별히 처리한다 (콜론 이스케이프).
`swift-argument-parser`가 Bash/Fish/Zsh 스크립트를 자동 생성하지만,
OCI 이미지명의 콜론 문제는 Zsh에서만 발생하기 때문이다.

### 10.5 Static 메서드 패턴 (Term)

`Term` 클래스는 인스턴스를 만들지 않고 모든 메서드가 `static`이다.
터미널은 프로세스당 하나이므로 싱글턴이 자연스럽지만,
상태를 내부에 보관하지 않고 `State` 구조체로 외부에 반환하여:
- 여러 곳에서 `MakeRaw`/`Restore`를 호출해도 올바르게 동작
- 상태 관리 책임이 호출자에게 있어 명시적

---

## 소스코드 참조 요약

| 파일 | 역할 | 줄수 |
|------|------|------|
| `Sources/tart/ShellCompletions/ShellCompletions.swift` | Zsh 자동완성 함수 | 29 |
| `Sources/tart/VNC/VNC.swift` | VNC 프로토콜 정의 | 6 |
| `Sources/tart/VNC/ScreenSharingVNC.swift` | macOS 화면 공유 VNC | 26 |
| `Sources/tart/VNC/FullFledgedVNC.swift` | VZ 프레임워크 VNC 서버 | 38 |
| `Sources/tart/Serial.swift` | PTY 기반 시리얼 콘솔 | 44 |
| `Sources/tart/Term.swift` | 터미널 제어 (Raw 모드) | 60 |
| `Sources/tart/Passphrase/PassphraseGenerator.swift` | 패스프레이즈 시퀀스 | 13 |
| `Sources/tart/Passphrase/Words.swift` | BIP-39 단어 목록 | ~2048 |
| `Sources/tart/Utils.swift` | 유틸리티 함수 | 24 |
| `Sources/tart/Commands/Run.swift` | VNC/Serial 통합 | ~1150 |

---

## 핵심 정리

```
┌────────────────────────────────────────────────────────────────┐
│  Shell Completions                                              │
│  ├── completeMachines: Local + OCI 전체                         │
│  ├── completeLocalMachines: 로컬만 (run, set, delete)           │
│  ├── completeRunningMachines: 실행 중만 (stop, suspend, ip)     │
│  └── normalizeName: Zsh 콜론 이스케이프                         │
│                                                                  │
│  VNC 추상화                                                      │
│  ├── VNC Protocol: waitForURL + stop                            │
│  ├── ScreenSharingVNC: 게스트 화면 공유 → vnc://IP              │
│  └── FullFledgedVNC: _VZVNCServer → vnc://:pass@127.0.0.1:port │
│                                                                  │
│  Serial 콘솔                                                     │
│  ├── createPTY: openpty → nonblocking → 115200 baud             │
│  └── VZVirtioConsoleDeviceSerialPortConfiguration 연결           │
│                                                                  │
│  터미널 제어 (Term)                                              │
│  ├── IsTerminal: tcgetattr로 터미널 여부 감지                    │
│  ├── MakeRaw: cfmakeraw로 Raw 모드 전환                         │
│  ├── Restore: 원래 termios 복원                                  │
│  └── GetSize: TIOCGWINSZ ioctl로 창 크기 조회                   │
│                                                                  │
│  PassphraseGenerator                                             │
│  ├── BIP-39 2048 단어, arc4random_uniform                       │
│  └── Sequence 프로토콜로 무한 스트림                              │
│                                                                  │
│  Utils                                                           │
│  ├── Collection[safe:]: 안전한 인덱스 접근                       │
│  └── resolveBinaryPath: PATH 환경변수에서 바이너리 검색          │
└────────────────────────────────────────────────────────────────┘
```

---

*작성일: 2026-03-08*
*소스 기준: /Users/ywlee/sideproejct/CNCF/tart/*
