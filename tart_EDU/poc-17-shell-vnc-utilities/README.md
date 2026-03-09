# PoC: Tart Shell Completions, VNC 원격 접근, 유틸리티 시뮬레이션

## 개요

Tart의 Shell Completions, VNC 원격 접근 (ScreenSharing/FullFledged),
Serial 콘솔 (PTY), PassphraseGenerator, 터미널 제어, 유틸리티 함수를
Go 표준 라이브러리만으로 시뮬레이션한다.

## 대응하는 Tart 소스코드

| 이 PoC | Tart 소스 | 설명 |
|--------|----------|------|
| `completeMachines()` | `ShellCompletions/ShellCompletions.swift` | 전체 VM 자동완성 |
| `completeRunningMachines()` | `ShellCompletions/ShellCompletions.swift` | 실행 중 VM만 |
| `normalizeName()` | `ShellCompletions/ShellCompletions.swift` | Zsh 콜론 이스케이프 |
| `VNC` interface | `VNC/VNC.swift` | VNC 프로토콜 추상화 |
| `ScreenSharingVNC` | `VNC/ScreenSharingVNC.swift` | macOS 화면 공유 VNC |
| `FullFledgedVNC` | `VNC/FullFledgedVNC.swift` | VZ 프레임워크 VNC 서버 |
| `CreatePTY()` | `Serial.swift` | PTY 생성 (openpty) |
| `Term` | `Term.swift` | 터미널 Raw 모드, 크기 조회 |
| `PassphraseGenerator` | `Passphrase/PassphraseGenerator.swift` | BIP-39 단어 기반 |
| `SafeIndex()` | `Utils.swift` | 안전한 컬렉션 접근 |
| `ResolveBinaryPath()` | `Utils.swift` | PATH 바이너리 검색 |
| `DetermineRunMode()` | `Commands/Run.swift` | VNC/Serial/Graphics 조합 |

## 구현 내용

### 1. Shell Completions
- completeMachines: Local + OCI 스토리지 합산
- completeLocalMachines: 로컬 VM만
- completeRunningMachines: 실행 상태 필터링
- normalizeName: Zsh 콜론 이스케이프

### 2. VNC Protocol 추상화
- VNC interface: WaitForURL + Stop
- ScreenSharingVNC: MAC→IP 해석 (DHCP/ARP), vnc://IP
- FullFledgedVNC: 패스프레이즈 인증, 임의 포트, 폴링

### 3. Serial 콘솔
- PTY 생성: openpty → close slave → O_NONBLOCK → 115200 baud
- Non-blocking 읽기/쓰기

### 4. 터미널 제어
- IsTerminal: tcgetattr 기반 감지
- MakeRaw: cfmakeraw (ECHO/ICANON/ISIG 비활성화)
- Restore: 원래 termios 복원
- GetSize: TIOCGWINSZ ioctl

### 5. PassphraseGenerator
- BIP-39 단어 목록 (2048개)
- Sequence 패턴으로 무한 스트림
- 4단어 하이픈 결합

### 6. Utils
- SafeIndex: 범위 밖 → nil (크래시 방지)
- ResolveBinaryPath: PATH 환경변수 순회

### 7. Run 옵션 조합
- --vnc vs --vnc-experimental 상호 배타
- --no-graphics + --vnc 조합
- --serial PTY 자동 생성

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- VNC Protocol 추상화로 ScreenSharing/FullFledged 두 구현을 동일 인터페이스로 처리한다
- FullFledgedVNC는 비공개 API(_VZVNCServer)를 사용하여 복구 모드/설치 화면도 원격 관찰 가능하다
- BIP-39 단어 목록으로 기억하기 쉽고 보안 강도 충분한 패스프레이즈를 생성한다
- PTY 기반 시리얼 콘솔은 SSH 대안으로 네트워크 없이도 VM 접근을 가능하게 한다
- Zsh 콜론 이스케이프는 OCI 이미지명의 태그 구분자가 자동완성을 깨뜨리는 문제를 해결한다
