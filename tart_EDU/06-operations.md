# 06. Tart 운영 가이드

## 1. 설치

### Homebrew (권장)

```bash
brew install cirruslabs/cli/tart
```

### 소스에서 빌드

```bash
git clone https://github.com/cirruslabs/tart.git
cd tart
swift build -c release
# .build/release/tart 바이너리 생성
```

### 요구사항

| 항목 | 최소 요구 |
|------|----------|
| macOS | 13.0 (Ventura) 이상 |
| 하드웨어 | Apple Silicon (M1/M2/M3/M4) |
| 디스크 | VM당 약 25~80 GB |
| 메모리 | VM 할당량 + 호스트 여유분 |

## 2. 기본 사용법

### macOS VM 생성 및 실행

```bash
# 최신 IPSW로 macOS VM 생성 (25GB+ 다운로드)
tart create my-macos --from-ipsw latest --disk-size 80

# 기존 이미지 클론 (APFS CoW, 빠름)
tart clone ghcr.io/cirruslabs/macos-tahoe-base:latest my-tahoe

# VM 실행 (GUI 창 열림)
tart run my-tahoe

# VM 실행 (headless)
tart run my-tahoe --no-graphics

# VNC으로 연결
tart run my-tahoe --vnc
```

### Linux VM 생성 및 실행

```bash
# Linux VM 생성
tart create my-linux --linux --disk-size 50

# VM 실행
tart run my-linux

# ISO 마운트하여 실행 (설치)
tart run my-linux --disk /path/to/ubuntu.iso
```

### VM 설정 변경

```bash
# CPU/메모리 변경 (VM 중지 상태에서)
tart set my-vm --cpu 8 --memory 16384

# 디스크 크기 확장
tart set my-vm --disk-size 100

# 디스플레이 해상도 변경
tart set my-vm --display 1920x1080

# MAC 주소 재생성
tart set my-vm --random-mac
```

### VM 관리

```bash
# VM 목록 (로컬 + OCI 캐시)
tart list

# VM 설정 조회
tart get my-vm

# VM 중지
tart stop my-vm

# VM 일시정지 (macOS 14+)
tart suspend my-vm

# VM 삭제
tart delete my-vm

# VM 이름 변경
tart rename old-name new-name

# VM IP 주소 조회
tart ip my-vm
```

## 3. OCI 레지스트리 연동

### 레지스트리 인증

```bash
# Keychain에 인증 정보 저장
tart login ghcr.io

# 환경변수 방식 (CI/CD)
export TART_REGISTRY_USERNAME=user
export TART_REGISTRY_PASSWORD=token
```

### Push/Pull

```bash
# Pull (OCI 레지스트리에서 VM 다운로드)
tart pull ghcr.io/cirruslabs/macos-tahoe-base:latest

# Push (VM을 레지스트리에 업로드)
tart push my-vm ghcr.io/myorg/my-image:v1

# 여러 태그로 동시 Push
tart push my-vm ghcr.io/myorg/my-image:v1 ghcr.io/myorg/my-image:latest

# 청크 업로드 (AWS ECR: 5MB+, GHCR: 4MB-)
tart push my-vm ghcr.io/myorg/my-image:v1 --chunk-size 5

# 병렬 다운로드 동시성 조정
tart pull ghcr.io/cirruslabs/macos-tahoe-base:latest --concurrency 8
```

### Docker config 호환

`~/.docker/config.json`의 인증 정보를 자동으로 사용합니다:

```json
{
  "auths": {
    "ghcr.io": {
      "auth": "base64(username:password)"
    }
  },
  "credHelpers": {
    "ghcr.io": "osxkeychain"
  }
}
```

## 4. 내보내기/가져오기

### Apple Archive 형식

```bash
# 내보내기 (LZFSE 압축)
tart export my-vm /path/to/my-vm.aar

# 가져오기
tart import my-vm /path/to/my-vm.aar
```

## 5. Guest Agent (tart exec)

### 요구사항

VM 내부에 Tart Guest Agent가 설치되어 있어야 합니다. Cirrus Labs 공식 이미지에는 기본 포함.

### 사용법

```bash
# 기본 명령 실행
tart exec my-vm -- ls -la

# 대화형 모드 (-i)
tart exec -i my-vm -- /bin/bash

# PTY 할당 (-t)
tart exec -it my-vm -- /bin/bash

# 파이프 입력
echo "hello" | tart exec -i my-vm -- cat
```

### 동작 원리

```
호스트                              게스트
tart exec ──── Unix 소켓 ──── Guest Agent
   │       (control.sock)         │
   ├── gRPC 채널                   ├── 명령 실행
   ├── stdin 스트리밍               ├── stdout/stderr 스트리밍
   └── 터미널 크기 동기화            └── exit code 반환
```

## 6. 환경변수

| 환경변수 | 기본값 | 설명 |
|---------|--------|------|
| `TART_HOME` | `~/.tart` | Tart 홈 디렉토리 |
| `TART_REGISTRY_USERNAME` | (없음) | 레지스트리 사용자명 |
| `TART_REGISTRY_PASSWORD` | (없음) | 레지스트리 비밀번호 |
| `TART_NO_AUTO_PRUNE` | (없음) | 자동 프루닝 비활성화 |
| `CIRRUS_SENTRY_TAGS` | (없음) | OpenTelemetry 태그 |

## 7. 캐시 관리

### 자동 프루닝

Pull/Clone 시 디스크 공간이 부족하면 자동으로 오래된 캐시를 정리합니다.

```bash
# 자동 프루닝 비활성화
export TART_NO_AUTO_PRUNE=1
```

### 수동 프루닝

```bash
# 7일 이상 접근하지 않은 캐시 삭제
tart prune --older-than 7

# OCI/IPSW 캐시를 50GB로 축소
tart prune --space-budget 50

# 로컬 VM 정리 (캐시가 아닌 VM)
tart prune --entries vms --older-than 30

# 캐시 + 기간 + 크기 복합 조건
tart prune --older-than 14 --space-budget 100
```

### 캐시 구조

```
~/.tart/cache/OCIs/
├── ghcr.io/cirruslabs/macos-tahoe-base/
│   ├── latest → sha256:abc...       # 태그 (심볼릭 링크)
│   └── sha256:abc.../               # 실제 데이터 (다이제스트)
│       ├── config.json
│       ├── disk.img
│       ├── nvram.bin
│       └── manifest.json
```

- **태그 기반**: 심볼릭 링크 → 다이제스트 디렉토리
- **GC**: 참조가 없는 다이제스트 디렉토리 자동 삭제
- **명시적 Pull**: `tart pull host/repo@sha256:...`로 Pull한 이미지는 GC 제외

## 8. 네트워크 설정

### Shared (NAT, 기본)

```bash
tart run my-vm  # 기본 NAT 네트워크
```

- 호스트와 같은 네트워크 인터페이스 공유
- DHCP로 IP 자동 할당
- 외부 접근 제한

### Bridged

```bash
# 특정 인터페이스에 브리지
tart run my-vm --net-bridged en0
```

- 물리 네트워크에 직접 연결
- 외부에서 접근 가능한 IP 할당
- root 권한 불필요 (macOS 13+)

### Softnet

```bash
# Softnet 사용 (root 권한 필요, 첫 실행 시 SUID 설정)
tart run my-vm --net-softnet
```

- 외부 `softnet` 프로세스 필요 (`brew install cirruslabs/cli/softnet`)
- Unix 소켓 페어로 패킷 전달
- 고급 네트워크 기능 지원

## 9. 디스크 포맷

### RAW (기본)

```bash
tart create my-vm --from-ipsw latest --disk-format raw
```

- 단순한 바이트 배열
- APFS CoW로 효율적 클론
- 모든 macOS 버전 지원

### ASIF (Apple Sparse Image Format)

```bash
tart create my-vm --from-ipsw latest --disk-format asif
```

- macOS 26 (Tahoe) 이상 필요
- 더 나은 성능
- diskutil로 리사이즈

## 10. 트러블슈팅

### VM 시작 실패

| 증상 | 원인 | 해결 |
|------|------|------|
| `UnsupportedArchitectureError` | x86_64에서 macOS VM 시도 | Apple Silicon 필요 |
| `UnsupportedHostOSError` | 호스트 macOS 버전 부족 | macOS 업그레이드 |
| `VMIsRunning` | 이미 실행 중인 VM 조작 | `tart stop` 먼저 실행 |
| `VMMissingFiles` | VM 파일 손상 | VM 재생성 또는 Pull |

### 네트워크 문제

| 증상 | 원인 | 해결 |
|------|------|------|
| IP 조회 실패 | VM 부팅 미완료 | `--wait` 옵션 사용 |
| Softnet SUID 오류 | 권한 부족 | `sudo` 비밀번호 입력 |
| 브리지 실패 | 인터페이스 없음 | `--net-bridged` 인터페이스 확인 |

### OCI 레지스트리 문제

| 증상 | 원인 | 해결 |
|------|------|------|
| `AuthFailed` | 인증 정보 없음 | `tart login` 또는 환경변수 설정 |
| `UnexpectedHTTPStatusCode` | 레지스트리 문제 | 레지스트리 URL/포트 확인 |
| Pull 속도 느림 | 동시성 낮음 | `--concurrency 8` 옵션 |
| Push 실패 | 청크 크기 불일치 | `--chunk-size` 레지스트리에 맞게 조정 |

### 디스크 공간 부족

```bash
# 1. 캐시 정리
tart prune --space-budget 50

# 2. 미사용 VM 삭제
tart list
tart delete unused-vm

# 3. 자동 프루닝 확인
echo $TART_NO_AUTO_PRUNE  # 비어있어야 자동 프루닝 활성
```

## 11. CI/CD 통합

### GitHub Actions (Cirrus Runners)

```yaml
jobs:
  build:
    runs-on: macos-latest-xlarge  # Cirrus Runner (Apple Silicon)
    steps:
      - uses: actions/checkout@v4
      - run: |
          tart clone ghcr.io/cirruslabs/macos-tahoe-xcode:latest runner
          tart run --no-graphics runner
```

### 일반 CI 파이프라인

```bash
#!/bin/bash
# VM 준비
tart clone ghcr.io/cirruslabs/macos-tahoe-base:latest ci-runner
tart set ci-runner --cpu 8 --memory 16384

# VM 실행 (백그라운드)
tart run --no-graphics ci-runner &
VM_PID=$!

# Guest Agent로 빌드 명령 실행
tart exec ci-runner -- xcodebuild -scheme MyApp -destination 'platform=macOS'

# 정리
tart stop ci-runner
tart delete ci-runner
```

## 12. OpenTelemetry 텔레메트리

Tart는 OpenTelemetry를 통해 모든 커맨드의 실행을 추적합니다.

### 추적 대상

- 각 커맨드 실행을 루트 스팬으로 생성
- Pull/Push 작업의 하위 스팬
- 프루닝 이벤트
- 에러 캡처 (`recordException`)

### 활성화

OpenTelemetry HTTP 엑스포터가 기본 포함되어 있으며, 표준 OTLP 환경변수로 설정합니다:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
```
