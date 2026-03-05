# 07. Apple Virtualization.Framework 심화

## 목차

1. [개요](#1-개요)
2. [VM 클래스 아키텍처](#2-vm-클래스-아키텍처)
3. [craftConfiguration: VZ 디바이스 조립 전체 흐름](#3-craftconfiguration-vz-디바이스-조립-전체-흐름)
4. [부트로더: VZMacOSBootLoader vs VZEFIBootLoader](#4-부트로더-vzmacosbootloader-vs-vzefibootloader)
5. [플랫폼: VZMacPlatformConfiguration vs VZGenericPlatformConfiguration](#5-플랫폼-vzmacplatformconfiguration-vs-vzgenericplatformconfiguration)
6. [그래픽스: VZMacGraphicsDeviceConfiguration vs VZVirtioGraphicsDeviceConfiguration](#6-그래픽스-vzmacgraphicsdeviceconfiguration-vs-vzvirtioGraphicsdeviceconfiguration)
7. [오디오: VZVirtioSoundDevice](#7-오디오-vzvirtiosounddevice)
8. [저장소: VZDiskImageStorageDeviceAttachment](#8-저장소-vzdiskimagestoragedeviceattachment)
9. [네트워크: VZNetworkDeviceAttachment](#9-네트워크-vznetworkdeviceattachment)
10. [콘솔 및 클립보드: VZSpiceAgentPortAttachment](#10-콘솔-및-클립보드-vzspiceagentportattachment)
11. [소켓: VZVirtioSocketDevice](#11-소켓-vzvirtiosocketdevice)
12. [키보드 및 포인팅 디바이스](#12-키보드-및-포인팅-디바이스)
13. [Suspendable 모드 제약사항](#13-suspendable-모드-제약사항)
14. [VZVirtualMachineDelegate: 생명주기 콜백](#14-vzvirtualmachinedelegate-생명주기-콜백)
15. [VM 생성 흐름: macOS vs Linux](#15-vm-생성-흐름-macos-vs-linux)
16. [VMConfig: 설정 직렬화와 역직렬화](#16-vmconfig-설정-직렬화와-역직렬화)
17. [설계 결정과 트레이드오프](#17-설계-결정과-트레이드오프)

---

## 1. 개요

Tart는 Apple의 Virtualization.Framework를 감싸는 Swift 래퍼로, macOS와 Linux 가상 머신을 Apple Silicon(arm64) 위에서 실행한다. 핵심 클래스 `VM`(`Sources/tart/VM.swift`)은 `VZVirtualMachine` 인스턴스를 생성하고 관리하며, 모든 가상 디바이스의 조립은 `craftConfiguration()` 정적 메서드에서 이루어진다.

Virtualization.Framework는 Apple이 macOS 11(Big Sur)부터 제공하는 하이퍼바이저 프레임워크로, 커널 레벨의 가상화 엔진(Hypervisor.framework) 위에서 고수준 API를 제공한다. Tart는 이 프레임워크의 거의 모든 기능을 활용한다:

```
+--------------------------------------------------------------------+
|                          Tart CLI                                  |
|  (Run, Create, Clone, Suspend, IP 등 커맨드)                       |
+--------------------------------------------------------------------+
           |
           v
+--------------------------------------------------------------------+
|                      VM 클래스 (VM.swift)                           |
|  - craftConfiguration(): VZ 디바이스 조립                           |
|  - VZVirtualMachineDelegate 구현                                   |
|  - start/stop/run 생명주기 관리                                    |
+--------------------------------------------------------------------+
           |
           v
+--------------------------------------------------------------------+
|              Platform 프로토콜 (Platform.swift)                     |
|  - Darwin: macOS 전용 (arm64 only)                                 |
|  - Linux: EFI 기반 범용 플랫폼                                     |
+--------------------------------------------------------------------+
           |
           v
+--------------------------------------------------------------------+
|            Apple Virtualization.Framework                           |
|  VZVirtualMachine / VZVirtualMachineConfiguration                  |
|  VZMacOSBootLoader / VZEFIBootLoader                               |
|  VZMacPlatformConfiguration / VZGenericPlatformConfiguration       |
|  VZVirtioBlockDeviceConfiguration / VZVirtioNetworkDevice 등       |
+--------------------------------------------------------------------+
           |
           v
+--------------------------------------------------------------------+
|              macOS Hypervisor.framework                             |
|  하드웨어 가속 가상화 (Apple Silicon)                               |
+--------------------------------------------------------------------+
```

---

## 2. VM 클래스 아키텍처

`VM` 클래스는 `NSObject`, `VZVirtualMachineDelegate`, `ObservableObject`를 상속/채택한다.

**파일 경로**: `Sources/tart/VM.swift` (25행)

```swift
class VM: NSObject, VZVirtualMachineDelegate, ObservableObject {
  @Published var virtualMachine: VZVirtualMachine
  var configuration: VZVirtualMachineConfiguration
  var sema = AsyncSemaphore(value: 0)
  var name: String
  var config: VMConfig
  var network: Network
  ...
}
```

### 핵심 프로퍼티

| 프로퍼티 | 타입 | 역할 |
|---------|------|------|
| `virtualMachine` | `VZVirtualMachine` | Virtualization.Framework의 실제 VM 인스턴스 |
| `configuration` | `VZVirtualMachineConfiguration` | VM의 전체 하드웨어 설정 |
| `sema` | `AsyncSemaphore` | Delegate 콜백과 비동기 흐름 동기화 |
| `name` | `String` | VM 디렉토리명 (사용자 정의 이름) |
| `config` | `VMConfig` | JSON에서 로드된 VM 설정 (CPU, 메모리, MAC 등) |
| `network` | `Network` | 네트워크 구현체 (Shared/Bridged/Softnet) |

### 생성자 시그니처

`VM` 클래스는 두 가지 생성자를 갖는다:

1. **기존 VM 실행용 (`init(vmDir:network:...)`)**
   - `Sources/tart/VM.swift` 43~86행
   - VMDirectory에서 config.json을 읽어 VM을 구성한다
   - `craftConfiguration()`을 호출하여 VZ 설정을 조립한다

2. **macOS VM 신규 생성용 (`init(vmDir:ipswURL:diskSizeGB:...)`)**
   - `Sources/tart/VM.swift` 146~229행
   - `#if arch(arm64)` 조건부 컴파일로 arm64에서만 사용 가능
   - IPSW 파일을 로드하고, `VZMacOSRestoreImage`로 하드웨어 요구사항을 파악
   - NVRAM 생성, 디스크 생성, config 초기화 후 `VZMacOSInstaller`로 OS 설치

---

## 3. craftConfiguration: VZ 디바이스 조립 전체 흐름

`craftConfiguration()`은 `VM` 클래스의 정적 메서드로, 모든 가상 디바이스를 조립하여 `VZVirtualMachineConfiguration`을 반환한다.

**파일 경로**: `Sources/tart/VM.swift` 309~445행

```
craftConfiguration() 디바이스 조립 순서
=============================================

1. Boot Loader          ← platform.bootLoader(nvramURL:)
2. CPU/Memory           ← vmConfig.cpuCount, vmConfig.memorySize
3. Platform             ← platform.platform(nvramURL:, needsNestedVirtualization:)
4. Graphics             ← platform.graphicsDevice(vmConfig:)
5. Audio                ← VZVirtioSoundDeviceConfiguration
6. Keyboard/Pointing    ← platform.keyboards(), platform.pointingDevices()
7. Network              ← network.attachments()
8. Clipboard (Spice)    ← VZSpiceAgentPortAttachment
9. Storage              ← VZDiskImageStorageDeviceAttachment
10. Entropy             ← VZVirtioEntropyDeviceConfiguration
11. Directory Sharing   ← directorySharingDevices
12. Serial Port         ← serialPorts
13. Version Console     ← "tart-version-{version}" 포트
14. Socket              ← VZVirtioSocketDeviceConfiguration
15. Validate            ← configuration.validate()
```

각 단계를 다이어그램으로 표현하면:

```
+--------------------------------------------------+
| VZVirtualMachineConfiguration                    |
|                                                  |
|  bootLoader ─────── [macOS: VZMacOSBootLoader]   |
|                      [Linux: VZEFIBootLoader]    |
|                                                  |
|  cpuCount ──────── vmConfig.cpuCount             |
|  memorySize ────── vmConfig.memorySize           |
|                                                  |
|  platform ──────── [macOS: VZMacPlatform]        |
|                     [Linux: VZGenericPlatform]   |
|                                                  |
|  graphicsDevices ── [VZMacGraphics / VZVirtio]   |
|  audioDevices ───── [VZVirtioSoundDevice]        |
|  keyboards ──────── [VZUSBKeyboard + VZMacKbd]   |
|  pointingDevices ── [VZUSBPointing + VZTrackpad] |
|  networkDevices ─── [VZVirtioNetworkDevice]      |
|  consoleDevices ─── [Spice + Version Console]    |
|  storageDevices ─── [Root Disk + Additional]     |
|  entropyDevices ─── [VZVirtioEntropy]            |
|  directorySharingDevices ── [VirtioFS]           |
|  serialPorts ────── [VZVirtioConsolePort]        |
|  socketDevices ──── [VZVirtioSocketDevice]       |
+--------------------------------------------------+
```

### 매개변수 분석

```swift
static func craftConfiguration(
  diskURL: URL,                                        // 디스크 이미지 경로
  nvramURL: URL,                                       // NVRAM 파일 경로
  vmConfig: VMConfig,                                  // CPU/메모리/MAC 등 설정
  network: Network = NetworkShared(),                  // 네트워크 구현체
  additionalStorageDevices: [VZStorageDeviceConfiguration],  // 추가 디스크
  directorySharingDevices: [VZDirectorySharingDeviceConfiguration],  // 디렉토리 공유
  serialPorts: [VZSerialPortConfiguration],            // 시리얼 포트
  suspendable: Bool = false,                           // 일시 중지 가능 모드
  nested: Bool = false,                                // 중첩 가상화
  audio: Bool = true,                                  // 오디오 활성화
  clipboard: Bool = true,                              // 클립보드 공유
  sync: VZDiskImageSynchronizationMode = .full,        // 디스크 동기화 모드
  caching: VZDiskImageCachingMode? = nil,              // 디스크 캐싱 모드
  noTrackpad: Bool = false,                            // 트랙패드 비활성화
  noPointer: Bool = false,                             // 포인터 비활성화
  noKeyboard: Bool = false                             // 키보드 비활성화
) throws -> VZVirtualMachineConfiguration
```

---

## 4. 부트로더: VZMacOSBootLoader vs VZEFIBootLoader

부트로더는 `Platform` 프로토콜의 `bootLoader(nvramURL:)` 메서드를 통해 결정된다.

**파일 경로**: `Sources/tart/Platform/Platform.swift` (1~16행)

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

### macOS 부트로더 (Darwin)

**파일 경로**: `Sources/tart/Platform/Darwin.swift` 57~59행

```swift
func bootLoader(nvramURL: URL) throws -> VZBootLoader {
  VZMacOSBootLoader()
}
```

- `VZMacOSBootLoader`는 macOS IPSW에서 설치한 부트 이미지를 직접 로드한다
- NVRAM URL을 별도로 받지만, macOS 부트로더 자체에는 전달하지 않는다
- NVRAM은 `VZMacPlatformConfiguration`의 `auxiliaryStorage`로 별도 설정된다
- Apple Silicon의 Secure Boot 체인과 통합된다

### Linux 부트로더 (Linux)

**파일 경로**: `Sources/tart/Platform/Linux.swift` 9~14행

```swift
func bootLoader(nvramURL: URL) throws -> VZBootLoader {
  let result = VZEFIBootLoader()
  result.variableStore = VZEFIVariableStore(url: nvramURL)
  return result
}
```

- `VZEFIBootLoader`는 UEFI 표준 부팅을 제공한다
- `VZEFIVariableStore`를 통해 EFI 변수(부팅 순서, Secure Boot 키 등)를 NVRAM에 저장한다
- 리눅스 VM 생성 시 `VZEFIVariableStore(creatingVariableStoreAt:)`으로 빈 저장소를 초기화한다

### 비교

```
macOS 부팅 흐름:
+--------+     +------------------+     +------------------+
| IPSW   | --> | VZMacOSBootLoader| --> | macOS Kernel     |
| 설치   |     | (Secure Boot)    |     | (arm64 native)   |
+--------+     +------------------+     +------------------+
                      |
                      v
               +------------------+
               | VZMacAuxiliary   |
               | Storage (NVRAM)  |
               +------------------+

Linux 부팅 흐름:
+--------+     +------------------+     +------------------+
| ISO/   | --> | VZEFIBootLoader  | --> | GRUB/systemd-boot|
| 디스크 |     | (UEFI 표준)      |     | → Linux Kernel   |
+--------+     +------------------+     +------------------+
                      |
                      v
               +------------------+
               | VZEFIVariable    |
               | Store (NVRAM)    |
               +------------------+
```

---

## 5. 플랫폼: VZMacPlatformConfiguration vs VZGenericPlatformConfiguration

### macOS 플랫폼 (Darwin)

**파일 경로**: `Sources/tart/Platform/Darwin.swift` 61~79행

```swift
func platform(nvramURL: URL, needsNestedVirtualization: Bool) throws -> VZPlatformConfiguration {
  if needsNestedVirtualization {
    throw RuntimeError.VMConfigurationError(
      "macOS virtual machines do not support nested virtualization")
  }

  let result = VZMacPlatformConfiguration()
  result.machineIdentifier = ecid
  result.auxiliaryStorage = VZMacAuxiliaryStorage(url: nvramURL)

  if !hardwareModel.isSupported {
    throw UnsupportedHostOSError()
  }

  result.hardwareModel = hardwareModel
  return result
}
```

macOS 플랫폼은 세 가지 필수 요소를 가진다:

| 요소 | 타입 | 역할 |
|------|------|------|
| `machineIdentifier` | `VZMacMachineIdentifier` | ECID: 각 VM 인스턴스의 고유 식별자 |
| `auxiliaryStorage` | `VZMacAuxiliaryStorage` | NVRAM: 부팅 설정, 스피커 볼륨 등 비휘발성 데이터 |
| `hardwareModel` | `VZMacHardwareModel` | 가상 Mac의 하드웨어 모델 (IPSW 호환성 결정) |

`Darwin` 구조체는 `ecid`와 `hardwareModel`을 자체 프로퍼티로 가지고 있으며, JSON 직렬화 시 base64 인코딩한다:

```swift
struct Darwin: PlatformSuspendable {
  var ecid: VZMacMachineIdentifier
  var hardwareModel: VZMacHardwareModel
  ...
}
```

**중첩 가상화 미지원**: macOS VM은 중첩 가상화를 지원하지 않으므로, `needsNestedVirtualization`이 `true`이면 예외를 던진다.

### Linux 플랫폼 (Linux)

**파일 경로**: `Sources/tart/Platform/Linux.swift` 17~23행

```swift
func platform(nvramURL: URL, needsNestedVirtualization: Bool) throws -> VZPlatformConfiguration {
  let config = VZGenericPlatformConfiguration()
  if #available(macOS 15, *) {
    config.isNestedVirtualizationEnabled = needsNestedVirtualization
  }
  return config
}
```

- `VZGenericPlatformConfiguration`은 범용 가상 플랫폼으로, ECID나 하드웨어 모델이 필요 없다
- macOS 15(Sequoia)부터 M3 칩 이상에서 중첩 가상화를 지원한다
- 별도의 Machine Identifier가 없어 설정이 단순하다

---

## 6. 그래픽스: VZMacGraphicsDeviceConfiguration vs VZVirtioGraphicsDeviceConfiguration

### macOS 그래픽스

**파일 경로**: `Sources/tart/Platform/Darwin.swift` 82~105행

```swift
func graphicsDevice(vmConfig: VMConfig) -> VZGraphicsDeviceConfiguration {
  let result = VZMacGraphicsDeviceConfiguration()

  if (vmConfig.display.unit ?? .point) == .point, let hostMainScreen = NSScreen.main {
    let vmScreenSize = NSSize(width: vmConfig.display.width, height: vmConfig.display.height)
    result.displays = [
      VZMacGraphicsDisplayConfiguration(for: hostMainScreen, sizeInPoints: vmScreenSize)
    ]
    return result
  }

  result.displays = [
    VZMacGraphicsDisplayConfiguration(
      widthInPixels: vmConfig.display.width,
      heightInPixels: vmConfig.display.height,
      pixelsPerInch: 72
    )
  ]
  return result
}
```

`VZMacGraphicsDeviceConfiguration`의 두 가지 디스플레이 생성 모드:

1. **포인트(pt) 모드**: 호스트의 메인 화면 DPI를 기준으로 렌더링한다. Retina 디스플레이에서 자동 스케일링되므로 선명한 표시가 가능하다.

2. **픽셀(px) 모드**: 명시적으로 픽셀 크기와 PPI(72)를 지정한다. CI/CD 환경처럼 물리적 화면이 없을 때 사용한다.

### Linux 그래픽스

**파일 경로**: `Sources/tart/Platform/Linux.swift` 25~35행

```swift
func graphicsDevice(vmConfig: VMConfig) -> VZGraphicsDeviceConfiguration {
  let result = VZVirtioGraphicsDeviceConfiguration()
  result.scanouts = [
    VZVirtioGraphicsScanoutConfiguration(
      widthInPixels: vmConfig.display.width,
      heightInPixels: vmConfig.display.height
    )
  ]
  return result
}
```

- `VZVirtioGraphicsDeviceConfiguration`은 VirtIO GPU를 사용한다
- Linux 게스트의 virtio-gpu 드라이버가 이 가상 GPU와 통신한다
- DPI 개념 없이 순수 픽셀 기반으로 동작한다

### VMDisplayConfig

**파일 경로**: `Sources/tart/VMConfig.swift` 35~54행

```swift
struct VMDisplayConfig: Codable, Equatable {
  enum Unit: String, Codable {
    case point = "pt"
    case pixel = "px"
  }

  var width: Int = 1024
  var height: Int = 768
  var unit: Unit?
}
```

기본 해상도는 1024x768이며, `tart set` 커맨드로 변경 가능하다.

---

## 7. 오디오: VZVirtioSoundDevice

**파일 경로**: `Sources/tart/VM.swift` 343~358행

```swift
// Audio
let soundDeviceConfiguration = VZVirtioSoundDeviceConfiguration()

if audio && !suspendable {
  let inputAudioStreamConfiguration = VZVirtioSoundDeviceInputStreamConfiguration()
  let outputAudioStreamConfiguration = VZVirtioSoundDeviceOutputStreamConfiguration()

  inputAudioStreamConfiguration.source = VZHostAudioInputStreamSource()
  outputAudioStreamConfiguration.sink = VZHostAudioOutputStreamSink()

  soundDeviceConfiguration.streams = [inputAudioStreamConfiguration, outputAudioStreamConfiguration]
} else {
  // just a null speaker
  soundDeviceConfiguration.streams = [VZVirtioSoundDeviceOutputStreamConfiguration()]
}

configuration.audioDevices = [soundDeviceConfiguration]
```

### 오디오 설정 분기

```
                      audio && !suspendable?
                             |
              +---------+----------+
              | YES                | NO
              v                    v
  +-------------------------+   +-------------------------+
  | Input:  HostAudioInput  |   | Output only:            |
  | Output: HostAudioOutput |   | null speaker            |
  | (마이크 + 스피커)        |   | (소리 없음, 디바이스 존재)|
  +-------------------------+   +-------------------------+
```

왜 `suspendable` 모드에서 오디오를 비활성화하는가?

- `VZHostAudioInputStreamSource`와 `VZHostAudioOutputStreamSink`는 호스트의 CoreAudio와 직접 연결된다
- VM을 일시 중지(suspend)하고 복원(resume)할 때, 이러한 호스트 리소스 바인딩이 깨질 수 있다
- Virtualization.Framework의 Save/Restore 기능이 이런 동적 리소스를 지원하지 않기 때문이다
- 그래서 null speaker(출력 스트림만 있되 sink가 없는 구성)를 사용하여 게스트 OS가 "사운드 카드가 있다"고 인식하되 실제 출력은 하지 않게 한다

---

## 8. 저장소: VZDiskImageStorageDeviceAttachment

**파일 경로**: `Sources/tart/VM.swift` 401~414행

```swift
var attachment = try VZDiskImageStorageDeviceAttachment(
  url: diskURL,
  readOnly: false,
  cachingMode: caching ?? (vmConfig.os == .linux ? .cached : .automatic),
  synchronizationMode: sync
)

var devices: [VZStorageDeviceConfiguration] = [VZVirtioBlockDeviceConfiguration(attachment: attachment)]
devices.append(contentsOf: additionalStorageDevices)
configuration.storageDevices = devices
```

### 캐싱 모드 결정 로직

```
                    caching 매개변수 지정?
                           |
              +--------+--------+
              | YES              | NO
              v                  v
     지정된 값 사용          OS 확인
                              |
                  +-------+-------+
                  | Linux          | macOS
                  v                v
              .cached         .automatic
```

왜 Linux VM에 `.cached`가 기본값인가?

소스코드 주석에 따르면(`Sources/tart/VM.swift` 405~407행):

```swift
// When not specified, use "cached" caching mode for Linux VMs to prevent file-system corruption[1]
//
// [1]: https://github.com/cirruslabs/tart/pull/675
```

Linux의 파일 시스템(ext4 등)은 `automatic` 모드에서 데이터 손상이 발생할 수 있다. `.cached` 모드는 호스트 측에서 디스크 I/O를 캐싱하여 게스트 파일 시스템의 일관성을 보장한다.

### 동기화 모드 옵션

| 모드 | 설명 | 성능 | 안전성 |
|------|------|------|--------|
| `.full` | 모든 쓰기를 영구 저장소에 완전히 동기화 | 느림 | 최고 |
| `.fsync` | fsync 호출 시에만 동기화 | 중간 | 중간 |
| `.none` | 동기화 비활성화 | 빠름 | 낮음 |

### 추가 디스크

`Run.swift`에서 `--disk` 옵션으로 추가 디스크를 지정할 수 있으며, `additionalStorageDevices` 매개변수로 전달된다. 루트 디스크와 함께 `storageDevices` 배열에 추가된다.

---

## 9. 네트워크: VZNetworkDeviceAttachment

**파일 경로**: `Sources/tart/VM.swift` 382~387행

```swift
configuration.networkDevices = network.attachments().map {
  let vio = VZVirtioNetworkDeviceConfiguration()
  vio.attachment = $0
  vio.macAddress = vmConfig.macAddress
  return vio
}
```

네트워크 설정의 핵심은 `Network` 프로토콜의 `attachments()` 메서드가 반환하는 `VZNetworkDeviceAttachment` 배열이다. 각 attachment에 대해:

1. `VZVirtioNetworkDeviceConfiguration`을 생성한다
2. attachment를 할당한다
3. VMConfig에서 읽은 MAC 주소를 설정한다

```
Network 프로토콜 구현체별 Attachment 타입
=========================================

NetworkShared ──────→ VZNATNetworkDeviceAttachment
  └─ Apple의 내장 NAT/DHCP 사용

NetworkBridged ─────→ VZBridgedNetworkDeviceAttachment
  └─ 호스트의 물리 인터페이스에 직접 브릿지

Softnet ────────────→ VZFileHandleNetworkDeviceAttachment
  └─ Unix 소켓 + 외부 softnet 프로세스 사용
```

MAC 주소는 VM 생성 시 `VZMACAddress.randomLocallyAdministered()`로 자동 생성되며, `config.json`에 저장된다. `tart run` 시 같은 MAC 주소를 가진 다른 VM이 실행 중이면 자동으로 MAC 주소를 재생성한다.

---

## 10. 콘솔 및 클립보드: VZSpiceAgentPortAttachment

**파일 경로**: `Sources/tart/VM.swift` 389~399행

```swift
if clipboard {
  let spiceAgentConsoleDevice = VZVirtioConsoleDeviceConfiguration()
  let spiceAgentPort = VZVirtioConsolePortConfiguration()
  spiceAgentPort.name = VZSpiceAgentPortAttachment.spiceAgentPortName
  let spiceAgentPortAttachment = VZSpiceAgentPortAttachment()
  spiceAgentPortAttachment.sharesClipboard = true
  spiceAgentPort.attachment = spiceAgentPortAttachment
  spiceAgentConsoleDevice.ports[0] = spiceAgentPort
  configuration.consoleDevices.append(spiceAgentConsoleDevice)
}
```

### Spice Agent 콘솔 구조

```
+------------------+          +------------------+
|   호스트 macOS    |          |   게스트 VM       |
|                  |          |                  |
|  macOS 클립보드  |<-------->|  VZSpiceAgent    |
|  (NSPasteboard)  |  VirtIO  |  (spice-vdagent) |
|                  |  Console |  또는 tart-agent  |
+------------------+          +------------------+
```

- `VZSpiceAgentPortAttachment.spiceAgentPortName`: 잘 알려진 포트 이름으로, 게스트 에이전트가 이 이름으로 포트를 연결한다
- `sharesClipboard = true`: 양방향 클립보드 공유를 활성화한다
- Linux 게스트에서는 `spice-vdagent` 패키지가, macOS 게스트에서는 `tart-guest-agent`가 필요하다
- `--no-clipboard` 플래그로 비활성화 가능하다

### Version Console Device

**파일 경로**: `Sources/tart/VM.swift` 427~437행

```swift
let consolePort = VZVirtioConsolePortConfiguration()
consolePort.name = "tart-version-\(CI.version)"

let consoleDevice = VZVirtioConsoleDeviceConfiguration()
consoleDevice.ports[0] = consolePort

configuration.consoleDevices.append(consoleDevice)
```

이것은 데이터 전송용이 아닌 "기능 감지" 용도의 더미 콘솔이다. 게스트 에이전트가 이 포트 이름을 확인하여 호스트 Tart의 버전을 알 수 있다.

---

## 11. 소켓: VZVirtioSocketDevice

**파일 경로**: `Sources/tart/VM.swift` 439~440행

```swift
configuration.socketDevices = [VZVirtioSocketDeviceConfiguration()]
```

`VZVirtioSocketDevice`는 vsock(Virtio Socket) 기반의 호스트-게스트 통신 채널이다.

### 소켓 연결 메서드

**파일 경로**: `Sources/tart/VM.swift` 258~268행

```swift
@MainActor
func connect(toPort: UInt32) async throws -> VZVirtioSocketConnection {
  guard let socketDevice = virtualMachine.socketDevices.first else {
    throw RuntimeError.VMSocketFailed(toPort, ", VM has no socket devices configured")
  }

  guard let virtioSocketDevice = socketDevice as? VZVirtioSocketDevice else {
    throw RuntimeError.VMSocketFailed(toPort, ", expected VM's first socket device...")
  }

  return try await virtioSocketDevice.connect(toPort: toPort)
}
```

vsock의 주요 용도:

- Guest Agent와의 gRPC 통신 (IP 주소 조회 등)
- `ControlSocket`을 통한 제어 명령 수신 (`tart suspend`, `tart stop` 등)
- 네트워크와 독립적인 호스트-게스트 채널

```
Host (Tart CLI)                     Guest VM
+------------------+                +------------------+
|                  |                |                  |
|  connect(toPort) |  ←-- vsock --> |  Guest Agent     |
|  VZVirtioSocket  |    Channel    |  (Port Listener) |
|  Connection      |                |                  |
+------------------+                +------------------+
```

---

## 12. 키보드 및 포인팅 디바이스

### macOS 키보드 설정

**파일 경로**: `Sources/tart/Platform/Darwin.swift` 107~123행

```swift
func keyboards() -> [VZKeyboardConfiguration] {
  if #available(macOS 14, *) {
    return [VZUSBKeyboardConfiguration(), VZMacKeyboardConfiguration()]
  } else {
    return [VZUSBKeyboardConfiguration()]
  }
}

func keyboardsSuspendable() -> [VZKeyboardConfiguration] {
  if #available(macOS 14, *) {
    return [VZMacKeyboardConfiguration()]
  } else {
    return keyboards()
  }
}
```

| 타입 | 호환성 | 특징 |
|------|--------|------|
| `VZUSBKeyboardConfiguration` | 모든 OS | USB HID 표준 키보드, 범용 |
| `VZMacKeyboardConfiguration` | macOS 14+ | Mac 전용, 글로브 키 등 Mac 특수 키 지원 |

일반 모드에서는 USB + Mac 키보드를 모두 등록하여 호환성을 극대화하고, suspendable 모드에서는 Mac 키보드만 사용하여 Save/Restore와의 호환성을 보장한다.

### macOS 포인팅 디바이스

**파일 경로**: `Sources/tart/Platform/Darwin.swift` 125~142행

```swift
func pointingDevices() -> [VZPointingDeviceConfiguration] {
  [VZUSBScreenCoordinatePointingDeviceConfiguration(), VZMacTrackpadConfiguration()]
}

func pointingDevicesSimplified() -> [VZPointingDeviceConfiguration] {
  return [VZUSBScreenCoordinatePointingDeviceConfiguration()]
}

func pointingDevicesSuspendable() -> [VZPointingDeviceConfiguration] {
  if #available(macOS 14, *) {
    return [VZMacTrackpadConfiguration()]
  } else {
    return pointingDevices()
  }
}
```

| 타입 | 호환성 | 특징 |
|------|--------|------|
| `VZUSBScreenCoordinatePointingDeviceConfiguration` | 모든 OS | 절대 좌표 기반 마우스, VNC 호환 |
| `VZMacTrackpadConfiguration` | macOS 게스트 전용 | 멀티터치 제스처, 스크롤 등 지원 |

### Linux 입력 디바이스

**파일 경로**: `Sources/tart/Platform/Linux.swift` 38~49행

```swift
func keyboards() -> [VZKeyboardConfiguration] {
  [VZUSBKeyboardConfiguration()]
}

func pointingDevices() -> [VZPointingDeviceConfiguration] {
  [VZUSBScreenCoordinatePointingDeviceConfiguration()]
}
```

Linux는 USB 표준 디바이스만 사용한다. Mac 트랙패드와 Mac 키보드는 macOS 게스트 전용이다.

### craftConfiguration의 입력 디바이스 선택 로직

**파일 경로**: `Sources/tart/VM.swift` 361~379행

```swift
if suspendable, let platformSuspendable = vmConfig.platform.self as? PlatformSuspendable {
  configuration.keyboards = platformSuspendable.keyboardsSuspendable()
  configuration.pointingDevices = platformSuspendable.pointingDevicesSuspendable()
} else {
  if noKeyboard {
    configuration.keyboards = []
  } else {
    configuration.keyboards = vmConfig.platform.keyboards()
  }

  if noPointer {
    configuration.pointingDevices = []
  } else if noTrackpad {
    configuration.pointingDevices = vmConfig.platform.pointingDevicesSimplified()
  } else {
    configuration.pointingDevices = vmConfig.platform.pointingDevices()
  }
}
```

```
입력 디바이스 선택 흐름도
=========================

                  suspendable?
                       |
              +--------+--------+
              | YES              | NO
              v                  |
    PlatformSuspendable?         |
              |                  |
    +----+----+             +----+----+
    |YES      |NO           |         |
    v         v             v         |
  Suspendable  일반 모드     noKeyboard?|
  키보드/포인팅                |        |
                     +------+------+  |
                     |YES          |NO|
                     v             v  |
                   키보드=[]     일반키보드
                                      |
                                noPointer?
                                   |
                          +--------+--------+
                          |YES              |NO
                          v                 |
                     포인팅=[]          noTrackpad?
                                         |
                                +--------+--------+
                                |YES              |NO
                                v                 v
                           Simplified         전체 포인팅
                           (USB만)        (USB + 트랙패드)
```

---

## 13. Suspendable 모드 제약사항

Suspendable 모드(`--suspendable` 플래그 또는 이미 suspended 상태인 VM)에서는 여러 디바이스가 제한된다:

### 비활성화되는 디바이스

**1. 오디오 (`Sources/tart/VM.swift` 345행)**

```swift
if audio && !suspendable {
  // 호스트 오디오 입출력 연결
} else {
  // null speaker만
  soundDeviceConfiguration.streams = [VZVirtioSoundDeviceOutputStreamConfiguration()]
}
```

이유: `VZHostAudioInputStreamSource`/`VZHostAudioOutputStreamSink`는 CoreAudio 세션과 바인딩되며, VM 복원 시 이전 세션을 재구성할 수 없다.

**2. 엔트로피 디바이스 (`Sources/tart/VM.swift` 417~419행)**

```swift
if !suspendable {
  configuration.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
}
```

이유: `VZVirtioEntropyDeviceConfiguration`은 호스트의 `/dev/random`과 연결되는데, 이 연결 상태를 직렬화할 수 없다.

**3. 입력 디바이스 변경 (`Sources/tart/VM.swift` 361~363행)**

Suspendable 모드에서는 `PlatformSuspendable` 프로토콜의 `keyboardsSuspendable()`, `pointingDevicesSuspendable()`을 사용한다:
- USB 키보드 제외, Mac 키보드만 사용
- USB 포인팅 제외, Mac 트랙패드만 사용

### PlatformSuspendable 프로토콜

**파일 경로**: `Sources/tart/Platform/Platform.swift` 13~16행

```swift
protocol PlatformSuspendable: Platform {
  func pointingDevicesSuspendable() -> [VZPointingDeviceConfiguration]
  func keyboardsSuspendable() -> [VZKeyboardConfiguration]
}
```

현재 `Darwin`만 이 프로토콜을 채택한다. Linux는 Suspend/Resume을 지원하지 않는다.

### Suspend/Resume 흐름

```
tart run --suspendable
           |
           v
     VM 실행 중 ───────────────────────────────────→ 정상 종료
           |
      SIGUSR1 수신
      (tart suspend)
           |
           v
     pause() ──→ saveMachineStateTo(stateURL)
                        |
                        v
                  state.vzvmsave 파일 저장
                        |
                        v
                     VM 종료

다음 tart run 시:
     stateURL에 파일 존재 확인
           |
           v
     restoreMachineStateFrom(stateURL) ──→ resume()
```

`Run.swift` 464~473행에서 상태 파일 존재 여부를 확인하고, 있으면 복원한다:

```swift
if FileManager.default.fileExists(atPath: vmDir.stateURL.path) {
  print("restoring VM state from a snapshot...")
  try await vm!.virtualMachine.restoreMachineStateFrom(url: vmDir.stateURL)
  try FileManager.default.removeItem(at: vmDir.stateURL)
  resume = true
  print("resuming VM...")
}
```

---

## 14. VZVirtualMachineDelegate: 생명주기 콜백

`VM` 클래스는 `VZVirtualMachineDelegate`를 구현하여 VM의 생명주기 이벤트를 처리한다.

**파일 경로**: `Sources/tart/VM.swift` 447~460행

```swift
func guestDidStop(_ virtualMachine: VZVirtualMachine) {
  print("guest has stopped the virtual machine")
  sema.signal()
}

func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
  print("guest has stopped the virtual machine due to error: \(error)")
  sema.signal()
}

func virtualMachine(_ virtualMachine: VZVirtualMachine, networkDevice: VZNetworkDevice,
                     attachmentWasDisconnectedWithError error: Error) {
  print("virtual machine's network attachment \(networkDevice) has been disconnected with error: \(error)")
  sema.signal()
}
```

### 콜백 동작 메커니즘

```
VZVirtualMachine 상태 변화                     VM 클래스 반응
================================              ================

게스트가 shutdown 실행
     → guestDidStop() 호출 ─────────────────→ sema.signal()
                                                    |
VM 내부 오류 발생                                    |
     → didStopWithError() 호출 ────────────→ sema.signal()
                                                    |
네트워크 연결 끊김                                   |
     → attachmentWasDisconnected() 호출 ───→ sema.signal()
                                                    |
                                                    v
                                              run()에서 sema.wait()
                                              해제 → 정리 작업 수행
```

`sema` (AsyncSemaphore)는 `run()` 메서드에서 대기하고 있다(`Sources/tart/VM.swift` 270~286행):

```swift
func run() async throws {
  do {
    try await sema.waitUnlessCancelled()
  } catch is CancellationError {
    // Ctrl+C, tart stop, VM 윈도우 닫기
  }

  if Task.isCancelled {
    if (self.virtualMachine.state == VZVirtualMachine.State.running) {
      print("Stopping VM...")
      try await stop()
    }
  }

  try await network.stop()
}
```

세마포어가 signal되면:
1. 취소에 의한 것이면 VM을 정상 중지한다
2. 네트워크를 정리한다
3. `Run.swift`에서 프로세스를 종료한다

---

## 15. VM 생성 흐름: macOS vs Linux

### macOS VM 생성

**파일 경로**: `Sources/tart/VM.swift` 146~229행

```
macOS VM 생성 시퀀스
===================

1. IPSW URL 확인
   ├─ 원격 URL → retrieveIPSW()로 다운로드 + 캐싱
   └─ 로컬 파일 → 직접 사용

2. VZMacOSRestoreImage.load(from: ipswURL)
   └─ 복원 이미지에서 하드웨어 요구사항 추출

3. requirements = image.mostFeaturefulSupportedConfiguration
   └─ 현재 호스트에서 지원하는 가장 고급 설정 선택

4. VZMacAuxiliaryStorage(creatingStorageAt: nvramURL, hardwareModel: requirements.hardwareModel)
   └─ NVRAM 파일 생성

5. vmDir.resizeDisk(diskSizeGB, format: diskFormat)
   └─ 빈 디스크 이미지 생성

6. VMConfig 초기화
   └─ Darwin(ecid: VZMacMachineIdentifier(), hardwareModel: requirements.hardwareModel)
   └─ CPU 최소 4코어 보장: max(4, requirements.minimumSupportedCPUCount)

7. craftConfiguration() 호출
   └─ VZ 디바이스 조립

8. VZMacOSInstaller로 OS 설치
   └─ installer.install() 비동기 실행
```

주목할 점: CPU를 최소 4코어로 설정하는 이유가 주석에 명시되어 있다(`Sources/tart/VM.swift` 193행):

```swift
// allocate at least 4 CPUs because otherwise VMs are frequently freezing
try config.setCPU(cpuCount: max(4, requirements.minimumSupportedCPUCount))
```

### Linux VM 생성

**파일 경로**: `Sources/tart/VM.swift` 232~245행

```swift
@available(macOS 13, *)
static func linux(vmDir: VMDirectory, diskSizeGB: UInt16, diskFormat: DiskImageFormat = .raw) async throws -> VM {
  _ = try VZEFIVariableStore(creatingVariableStoreAt: vmDir.nvramURL)
  try vmDir.resizeDisk(diskSizeGB, format: diskFormat)

  let config = VMConfig(platform: Linux(), cpuCountMin: 4,
                         memorySizeMin: 4096 * 1024 * 1024, diskFormat: diskFormat)
  try config.save(toURL: vmDir.configURL)

  return try VM(vmDir: vmDir)
}
```

Linux VM 생성은 macOS에 비해 훨씬 단순하다:
- IPSW 불필요, UEFI Variable Store만 생성
- 하드웨어 요구사항 검사 불필요
- OS 설치 과정 없음 (사용자가 ISO로 직접 설치)
- 기본값: 4 CPU, 4GB RAM

---

## 16. VMConfig: 설정 직렬화와 역직렬화

**파일 경로**: `Sources/tart/VMConfig.swift`

`VMConfig`는 VM의 모든 설정을 `config.json` 파일로 직렬화/역직렬화한다.

### 구조체 필드

```swift
struct VMConfig: Codable {
  var version: Int = 1
  var os: OS                    // .darwin 또는 .linux
  var arch: Architecture        // .arm64 또는 .amd64
  var platform: Platform        // Darwin 또는 Linux
  var cpuCountMin: Int          // 최소 CPU 수
  private(set) var cpuCount: Int         // 실제 CPU 수
  var memorySizeMin: UInt64     // 최소 메모리 (바이트)
  private(set) var memorySize: UInt64    // 실제 메모리 (바이트)
  var macAddress: VZMACAddress  // VM MAC 주소
  var display: VMDisplayConfig  // 디스플레이 설정
  var displayRefit: Bool?       // 디스플레이 리피팅
  var diskFormat: DiskImageFormat  // 디스크 포맷 (raw/asif)
}
```

### 커스텀 Codable 구현

`VMConfig`는 `Codable`을 수동 구현한다. 특히 `Platform` 필드의 디코딩이 복잡하다:

```swift
init(from decoder: Decoder) throws {
  let container = try decoder.container(keyedBy: CodingKeys.self)
  ...
  switch os {
  case .darwin:
    #if arch(arm64)
      platform = try Darwin(from: decoder)
    #else
      throw DecodingError.dataCorruptedError(...)
    #endif
  case .linux:
    platform = try Linux(from: decoder)
  }
  ...
}
```

`os` 필드의 값에 따라 `Darwin` 또는 `Linux` 구조체를 선택적으로 디코딩한다. arm64가 아닌 아키텍처에서 Darwin VM의 config.json을 읽으면 에러가 발생한다.

### 리소스 검증

```swift
mutating func setCPU(cpuCount: Int) throws {
  if os == .darwin && cpuCount < cpuCountMin {
    throw LessThanMinimalResourcesError(...)
  }
  if cpuCount < VZVirtualMachineConfiguration.minimumAllowedCPUCount {
    throw LessThanMinimalResourcesError(...)
  }
  self.cpuCount = cpuCount
}
```

- macOS VM: IPSW에서 가져온 `cpuCountMin` 이상이어야 한다
- 모든 VM: Virtualization.Framework의 최소 허용값 이상이어야 한다
- `cpuCount`와 `memorySize`는 `private(set)`으로 setter를 통해서만 변경 가능하다

---

## 17. 설계 결정과 트레이드오프

### 왜 Platform 프로토콜로 추상화했는가?

`Platform` 프로토콜은 macOS와 Linux의 플랫폼 차이를 깔끔하게 캡슐화한다. `craftConfiguration()`은 OS에 따른 분기 없이 `vmConfig.platform.bootLoader()`, `vmConfig.platform.graphicsDevice()` 등을 호출한다. 새로운 OS 지원을 추가할 때 `Platform` 프로토콜만 구현하면 된다.

### 왜 모든 디바이스를 항상 등록하는가?

Tart는 사운드 디바이스를 비활성화할 때도 null speaker를 등록한다. 이는 게스트 OS가 사운드 카드의 존재를 기대하여 드라이버 로딩 실패 시 부팅이 느려지거나 에러 로그가 발생하는 것을 방지하기 위해서다.

### 왜 정적 메서드 craftConfiguration()인가?

`craftConfiguration()`을 정적 메서드로 만든 이유는 생성자(`init`)에서 설정을 조립할 때 `self`가 완전히 초기화되기 전에 호출해야 하기 때문이다. Swift에서 `self`의 모든 프로퍼티가 초기화되기 전에는 인스턴스 메서드를 호출할 수 없으므로, 정적 메서드로 분리하여 이 제약을 우회한다.

### 왜 AsyncSemaphore를 사용하는가?

`VZVirtualMachineDelegate`의 콜백은 메인 스레드에서 호출될 수 있고, `run()` 메서드는 Swift Concurrency의 비동기 컨텍스트에서 실행된다. `AsyncSemaphore`(`Semaphore` 패키지)를 사용하여 이 두 세계를 안전하게 연결한다. `sema.signal()`은 동기 컨텍스트(delegate)에서 호출되고, `sema.waitUnlessCancelled()`는 비동기 컨텍스트(`run()`)에서 대기한다.

### configuration.validate()의 역할

`craftConfiguration()`의 마지막 단계(`Sources/tart/VM.swift` 442행)에서 `try configuration.validate()`를 호출한다. 이 메서드는 Virtualization.Framework가 제공하는 검증으로, 설정의 일관성을 확인한다:
- CPU 수가 허용 범위 내인지
- 메모리 크기가 허용 범위 내인지
- 디바이스 조합이 유효한지
- 플랫폼과 부트로더가 호환되는지

이 검증이 실패하면 `VZError`가 발생하며, VM 인스턴스 생성 전에 문제를 발견할 수 있다.
