# PoC-09: 트리 기반 서브커맨드 파싱 시뮬레이션

## 개요

Tart의 CLI 파싱 구조를 Go로 재현한다. Swift ArgumentParser의 `AsyncParsableCommand` 프로토콜과 `CommandConfiguration`을 기반으로 한 트리형 서브커맨드 체계, 파라미터 파싱(`@Argument`, `@Option`, `@Flag`), 유효성 검증, 자동 도움말 생성, 셸 자동완성까지 시뮬레이션한다.

## Tart 소스코드 매핑

| Tart 소스 | PoC 대응 | 설명 |
|-----------|---------|------|
| `Root.swift` — `@main struct Root: AsyncParsableCommand` | `RootCommand` 구조체 | 루트 커맨드, 서브커맨드 트리 정의 |
| `CommandConfiguration(commandName:, subcommands:)` | `CommandConfiguration` 구조체 | 커맨드 메타데이터(이름, 설명, 하위 커맨드) |
| `@Argument`, `@Option`, `@Flag` | `ParamDef` 구조체 (`ParamArgument/ParamOption/ParamFlag`) | 파라미터 정의 |
| `parseAsRoot()` | `Parser.parseArgs()` | 인자 배열에서 서브커맨드 매칭 및 파라미터 추출 |
| `validate()` → `run()` 라이프사이클 | `Command.Validate()` → `Command.Run()` | 유효성 검증 후 실행 |
| `Clone.swift` — `validate()` | `CloneCommand.Validate()` | `new-name`에 `/` 불가, `concurrency >= 1` |
| `Prune.swift` — `validate()` | `PruneCommand.Validate()` | 최소 하나의 프루닝 기준 필요 |
| `ShellCompletions.swift` — `completeMachines()` | `ShellCompletion.Complete()` | 서브커맨드/옵션/VM 이름 자동완성 |

## 핵심 개념

### 1. 서브커맨드 트리 구조

Tart는 `Root → [Create, Clone, Run, Pull, Push, List, Prune, ...]` 형태의 1단계 트리 구조를 사용한다. `Root.swift`의 `CommandConfiguration.subcommands` 배열에 모든 서브커맨드를 등록한다.

```
tart (Root)
├── clone   — VM 복제
├── run     — VM 실행
├── pull    — OCI 레지스트리에서 이미지 가져오기
├── list    — VM 목록 표시
└── prune   — 캐시 정리
```

### 2. 파라미터 분류

| 타입 | Swift | Go PoC | 예시 |
|------|-------|--------|------|
| 위치 인자 | `@Argument` | `ParamArgument` | `tart clone <source> <new-name>` |
| 이름 기반 옵션 | `@Option` | `ParamOption` | `--concurrency 4` |
| 불리언 플래그 | `@Flag` | `ParamFlag` | `--insecure` |

### 3. validate() → run() 라이프사이클

Swift ArgumentParser는 파싱 완료 후 `validate()`를 호출하여 비즈니스 규칙을 검증하고, 통과 시 `run()`을 실행한다. 이 PoC에서는 `Validate()` → `Run()` 메서드로 동일한 패턴을 구현한다.

### 4. Root.main() 실행 흐름

```
main()
  ├── SIGINT 핸들러 설정 (Ctrl+C → task.cancel())
  ├── setlinebuf(stdout) — 라인 버퍼링
  ├── parseAsRoot() — 서브커맨드 매칭
  ├── OTel 루트 스팬 생성 (커맨드 이름)
  ├── Config().gc() — GC 수행 (Pull/Clone 제외)
  ├── validate() → run()
  ├── 에러 시 OpenTelemetry에 예외 기록
  └── OTel.shared.flush() — 트레이스 전송
```

## 실행 방법

```bash
cd poc-09-cli-parser
go run main.go
```

## 실행 결과 (요약)

```
=== PoC-09: 트리 기반 서브커맨드 파싱 시뮬레이션 ===

--- [데모 1] 루트 도움말 ---
개요: tart — macOS 및 Linux VM 관리 도구
사용법: tart <subcommand> [옵션...]
서브커맨드:
  clone   VM을 복제합니다
  list    VM 목록을 표시합니다
  ...

--- [데모 3] 정상적인 clone 실행 ---
입력: tart clone ghcr.io/cirruslabs/macos-sonoma-base:latest my-vm --concurrency 8
[Clone] 실행: ghcr.io/cirruslabs/macos-sonoma-base:latest → my-vm (동시성: 8, insecure: false)

--- [데모 4] 유효성 검증 실패 ---
유효성 오류: <new-name>은 로컬 이름이어야 합니다

--- [데모 9] 셸 자동완성 시뮬레이션 ---
  입력: (빈 입력)                  → 후보: [clone list prune pull run]
  입력: cl                        → 후보: [clone]
  입력: clone --                  → 후보: [--insecure --concurrency --prune-limit]
```

## 학습 포인트

1. **트리 기반 CLI 설계**: 인터페이스(프로토콜)로 커맨드를 추상화하고, 서브커맨드 배열로 트리를 구성하는 패턴
2. **선언적 파라미터 정의**: 각 커맨드가 자신의 파라미터를 선언하고, 파서가 자동으로 파싱/검증하는 구조
3. **validate() 분리**: 파싱 성공과 비즈니스 규칙 검증을 분리하여 명확한 에러 메시지 제공
4. **자동 도움말 생성**: 커맨드/파라미터 메타데이터로부터 일관된 도움말을 자동 생성
5. **셸 자동완성**: 부분 입력에 기반한 서브커맨드, 옵션, VM 이름 후보 생성
