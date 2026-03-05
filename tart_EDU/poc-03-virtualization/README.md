# PoC 03: 가상 머신 구성 빌더 패턴 시뮬레이션

## 개요

tart의 VM 구성 조립 과정을 Go로 시뮬레이션한다.
`VM.craftConfiguration()`은 VMConfig, Platform, Network 정보를 조합하여
VZVirtualMachineConfiguration을 빌드하는 핵심 메서드이다.

macOS(Darwin)와 Linux 플랫폼에 따라 부트로더, 그래픽, 키보드, 포인팅 디바이스 등이
달라지며, suspendable 모드에서는 일부 디바이스가 비활성화된다.

## 실행 방법

```bash
go run main.go
```

## 핵심 시뮬레이션 포인트

| 구성 요소 | 시뮬레이션 내용 | 실제 tart 동작 |
|-----------|----------------|---------------|
| Platform 인터페이스 | Darwin/Linux 분기 | protocol Platform: Codable |
| 부트로더 | MacOS vs EFI 분기 | VZMacOSBootLoader / VZEFIBootLoader |
| 그래픽 | Mac/Virtio 디바이스 | VZMacGraphicsDevice / VZVirtioGraphicsDevice |
| 오디오 | audio && !suspendable 조건 | VZVirtioSoundDeviceConfiguration |
| 키보드 | 일반/suspendable/noKeyboard 분기 | USB + Mac / Mac only / 없음 |
| 포인팅 | 일반/suspendable/noTrackpad/noPointer | USB + Trackpad / Trackpad only |
| 스토리지 | 캐싱 모드 Linux=cached, Darwin=automatic | VZDiskImageStorageDeviceAttachment |
| Suspendable | 일부 디바이스 비활성화 | PlatformSuspendable 프로토콜 |
| 중첩 가상화 | Linux 전용 (macOS 15+) | GenericPlatformConfiguration.isNestedVirtualizationEnabled |
| validate() | CPU/메모리/디바이스 필수 조건 검증 | VZVirtualMachineConfiguration.validate() |

## tart 실제 소스코드 참조 경로

- `Sources/tart/VM.swift` — `craftConfiguration()` 메서드: 전체 VM 구성 조립 (line 309-445)
- `Sources/tart/Platform/Platform.swift` — Platform 프로토콜, PlatformSuspendable 프로토콜
- `Sources/tart/Platform/Darwin.swift` — macOS 플랫폼: 부트로더, 그래픽, 키보드, 트랙패드
- `Sources/tart/Platform/Linux.swift` — Linux 플랫폼: EFI 부트, Virtio 그래픽, 중첩 가상화
- `Sources/tart/VMConfig.swift` — VM 설정 데이터 (cpuCount, memorySize, display 등)
