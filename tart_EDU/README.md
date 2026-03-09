# Tart 교육 자료 (EDU)

> Apple Silicon에서 macOS/Linux 가상 머신을 빌드, 실행, 관리하는 가상화 도구

## 프로젝트 개요

Tart는 Apple의 `Virtualization.Framework`를 활용하여 Apple Silicon(M1/M2/M3/M4) Mac에서
macOS 및 Linux VM을 네이티브에 가까운 성능으로 실행하는 CLI 도구입니다.

### 핵심 특징

- **네이티브 가상화**: Apple Virtualization.Framework 기반, 하드웨어 가속
- **OCI 레지스트리 통합**: Docker 레지스트리 호환 Push/Pull로 VM 이미지 배포
- **CI/CD 최적화**: Cirrus CI, GitHub Actions 등과 통합, 자동화 친화적
- **APFS Copy-on-Write**: 클론 시 실제 디스크 사용량 최소화
- **Guest Agent**: VM 내부 명령 실행 (`tart exec`) 지원

### 기술 스택

| 항목 | 기술 |
|------|------|
| 언어 | Swift 5.10 |
| 프레임워크 | Apple Virtualization.Framework |
| CLI | swift-argument-parser |
| 이미지 포맷 | OCI (Open Container Initiative) |
| 네트워크 | NAT (Shared), Bridged, Softnet |
| 압축 | LZ4 (디스크 레이어), LZFSE (아카이브) |
| 인증 | Keychain, Docker config, 환경변수 |
| 텔레메트리 | OpenTelemetry |

## 문서 목차

### 기본 문서

| # | 문서 | 내용 |
|---|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, 컴포넌트 관계, 초기화 흐름 |
| 02 | [데이터 모델](02-data-model.md) | VM, VMConfig, VMDirectory, OCI Manifest 등 핵심 구조체 |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | VM 생성, 실행, Pull/Push, Clone 등 주요 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, Swift Package Manager, 의존성 |
| 05 | [핵심 컴포넌트](05-core-components.md) | VM, VMDirectory, Registry, Network 등 핵심 동작 원리 |
| 06 | [운영 가이드](06-operations.md) | 설치, 설정, 모니터링, 트러블슈팅 |

### 심화 문서

| # | 문서 | 내용 |
|---|------|------|
| 07 | [Virtualization.Framework](07-virtualization-framework.md) | Apple 가상화 프레임워크 활용, VZ* 클래스 매핑 |
| 08 | [OCI 레지스트리](08-oci-registry.md) | OCI 프로토콜, Pull/Push, 인증, 레이어 관리 |
| 09 | [VM 스토리지](09-vm-storage.md) | Local/OCI 스토리지, VMDirectory, 디스크 포맷 |
| 10 | [네트워크](10-network.md) | Shared/Bridged/Softnet 네트워크 모드 |
| 11 | [디스크 관리](11-disk-management.md) | RAW/ASIF 포맷, LZ4 압축, 레이어 분할 |
| 12 | [인증 시스템](12-credentials.md) | Keychain, Docker config, 환경변수, Bearer/Basic 토큰 |
| 13 | [CLI 커맨드](13-cli-commands.md) | swift-argument-parser, 20개 서브커맨드 구조 |
| 14 | [플랫폼 추상화](14-platform.md) | Darwin/Linux 플랫폼, 부트로더, 디바이스 설정 |
| 15 | [캐시와 프루닝](15-cache-pruning.md) | OCI 캐시, IPSW 캐시, 자동 프루닝, GC |
| 16 | [Guest Agent & Exec](16-guest-agent.md) | gRPC 기반 Guest Agent, tart exec 구현 |
| 17 | [잠금과 동시성](17-locking.md) | FileLock, PIDLock, VM 실행 상태 관리 |
| 18 | [텔레메트리](18-telemetry.md) | OpenTelemetry 통합, 스팬, 이벤트 추적 |
| 19 | [Shell/VNC/유틸리티](19-shell-vnc-utilities.md) | Shell Completions, VNC 원격 접근, Serial PTY, 유틸리티 |

### PoC (Proof of Concept)

| # | PoC | 핵심 시뮬레이션 |
|---|-----|----------------|
| 01 | [아키텍처](poc-01-architecture/) | CLI 커맨드 디스패치, VM 라이프사이클 시뮬레이션 |
| 02 | [데이터 모델](poc-02-data-model/) | VMConfig, VMDirectory, OCIManifest 구조체 |
| 03 | [가상화 구성](poc-03-virtualization/) | 가상 머신 구성(CPU, 메모리, 디바이스) 빌더 패턴 |
| 04 | [OCI 레지스트리](poc-04-oci-registry/) | OCI 프로토콜 Pull/Push 시뮬레이션 |
| 05 | [VM 스토리지](poc-05-vm-storage/) | Local/OCI 이중 스토리지, 심볼릭 링크 |
| 06 | [네트워크 모드](poc-06-network/) | Shared/Bridged/Softnet 네트워크 추상화 |
| 07 | [디스크 레이어](poc-07-disk-layer/) | LZ4 압축, 청크 분할, 병렬 Pull/Push |
| 08 | [인증 체인](poc-08-credentials/) | 다중 인증 프로바이더 체인 |
| 09 | [CLI 파서](poc-09-cli-parser/) | 트리 기반 서브커맨드 파싱 |
| 10 | [플랫폼 추상화](poc-10-platform/) | Protocol 기반 Darwin/Linux 플랫폼 분기 |
| 11 | [캐시 프루닝](poc-11-cache-pruning/) | LRU 기반 캐시 정리, 디스크 공간 관리 |
| 12 | [Guest Agent](poc-12-guest-agent/) | Unix 소켓 기반 Guest-Host 명령 실행 |
| 13 | [잠금 메커니즘](poc-13-locking/) | flock/fcntl 기반 파일 잠금, PID 잠금 |
| 14 | [APFS Clone](poc-14-apfs-clone/) | Copy-on-Write 클론 시뮬레이션 |
| 15 | [아카이브](poc-15-archive/) | LZFSE 스타일 VM 내보내기/가져오기 |
| 16 | [텔레메트리](poc-16-telemetry/) | 스팬 기반 분산 추적 시뮬레이션 |
| 17 | [Shell/VNC/유틸리티](poc-17-shell-vnc-utilities/) | Shell Completions, VNC 이중 구현, Serial PTY, PassphraseGenerator |

## 소스코드 참조

- 소스 경로: `/Users/ywlee/sideproejct/CNCF/tart/`
- GitHub: https://github.com/cirruslabs/tart
- 언어: Swift 5.10
- 최소 요구: macOS 13.0 (Ventura), Apple Silicon
