# PoC-10: Protocol 기반 Darwin/Linux 플랫폼 분기 시뮬레이션

## 개요

tart는 `Platform` 프로토콜(인터페이스)을 통해 macOS(Darwin)와 Linux 가상머신의 플랫폼별 설정을 추상화한다.
이 PoC는 Platform/PlatformSuspendable 인터페이스, Darwin/Linux 구현체, VMConfig의 다형성 JSON 디코딩을 Go로 시뮬레이션한다.

### 핵심 시뮬레이션 포인트

1. **Platform 인터페이스**: OS(), BootLoader(), GraphicsDevice(), Keyboards(), PointingDevices() 메서드
2. **DarwinPlatform**: macOS 부트로더, Mac 그래픽(point/pixel 모드), USB+Mac 키보드, 트랙패드
3. **LinuxPlatform**: EFI 부트로더+변수 저장소, Virtio 그래픽, USB 키보드만
4. **PlatformSuspendable**: 일시정지 모드에서 제한된 디바이스 (Darwin만 구현)
5. **JSON 다형성 디코딩**: `os` 필드로 darwin/linux 분기하여 Platform 인스턴스 생성
6. **중첩 가상화**: Darwin은 미지원, Linux는 macOS 15+에서 지원

## 실행 방법

```bash
cd tart_EDU/poc-10-platform
go run main.go
```

## tart 실제 소스코드 참조

| 파일 | 설명 |
|------|------|
| `Sources/tart/Platform/Platform.swift` | Platform, PlatformSuspendable 프로토콜 정의 |
| `Sources/tart/Platform/Darwin.swift` | macOS 플랫폼 구현 (ecid, hardwareModel, Mac 그래픽/키보드/트랙패드) |
| `Sources/tart/Platform/Linux.swift` | Linux 플랫폼 구현 (EFI 부트로더, Virtio 그래픽, USB 전용) |
| `Sources/tart/Platform/OS.swift` | OS 열거형 (darwin, linux) |
| `Sources/tart/VMConfig.swift` | VMConfig 구조체, os 필드 기반 다형성 디코딩 |

## 설계 패턴

```
Platform (인터페이스)
├── DarwinPlatform (PlatformSuspendable 구현)
│   ├── macOS 부트로더
│   ├── Mac 그래픽 (point/pixel)
│   ├── USB + Mac 키보드
│   └── USB 포인팅 + 트랙패드
└── LinuxPlatform (Platform만 구현)
    ├── EFI 부트로더 + 변수 저장소
    ├── Virtio 그래픽
    ├── USB 키보드만
    └── USB 포인팅만
```
