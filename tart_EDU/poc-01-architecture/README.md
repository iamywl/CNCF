# PoC 01: CLI 커맨드 디스패치와 VM 라이프사이클 시뮬레이션

## 개요

tart의 CLI 아키텍처와 VM 라이프사이클 관리 패턴을 Go로 시뮬레이션한다.
tart는 Swift ArgumentParser 기반의 서브커맨드 구조를 사용하며,
`Root.main()` 함수에서 SIGINT 처리, GC, OpenTelemetry 스팬 생성, 커맨드 디스패치를 순서대로 수행한다.

## 실행 방법

```bash
go run main.go
```

## 핵심 시뮬레이션 포인트

| 구성 요소 | 시뮬레이션 내용 | 실제 tart 동작 |
|-----------|----------------|---------------|
| 커맨드 디스패치 | map 기반 커맨드 라우팅 | ArgumentParser의 subcommands 배열 |
| SIGINT 처리 | signal.Notify + context.Cancel | signal(SIGINT, SIG_IGN) + DispatchSource |
| GC | 임시 디렉토리 정리 | Config.gc() — FileLock.trylock() 후 삭제 |
| OTel 스팬 | 커맨드별 스팬 생성/종료 | OTel.shared.tracer.spanBuilder().startSpan() |
| VM 라이프사이클 | Created → Configured → Running → Stopped | VMDirectory.State enum |
| VM 실행 | context 기반 대기 + 취소 | sema.waitUnlessCancelled() |

## tart 실제 소스코드 참조 경로

- `Sources/tart/Root.swift` — @main 진입점, 서브커맨드 등록, SIGINT 처리, GC 호출
- `Sources/tart/Commands/Create.swift` — VM 생성 (임시 디렉토리 → VMStorageLocal.move)
- `Sources/tart/Commands/Run.swift` — VM 실행 (VM.start → VM.run → sema 대기)
- `Sources/tart/Commands/Stop.swift` — VM 중지 (ControlSocket 연결)
- `Sources/tart/Commands/Delete.swift` — VM 삭제 (PIDLock 확인 후 제거)
- `Sources/tart/Commands/Clone.swift` — VM 복제 (config/disk/nvram 복사 + MAC 재생성)
- `Sources/tart/Commands/List.swift` — VM 목록 (Local + OCI 스토리지)
- `Sources/tart/OTel.swift` — OpenTelemetry 트레이싱 초기화/flush
- `Sources/tart/Config.swift` — gc() 가비지 컬렉션, tartTmpDir 관리
