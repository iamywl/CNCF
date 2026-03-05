# PoC-01: Helm v4 아키텍처 (CLI 디스패치 + Action 패턴)

## 개요

Helm v4의 핵심 아키텍처 패턴을 Go 표준 라이브러리만으로 시뮬레이션합니다.

## 시뮬레이션하는 패턴

### 1. CLI 디스패치 (Cobra 패턴)
- **실제 소스**: `cmd/helm/helm.go`, `pkg/cmd/root.go`
- Helm은 `spf13/cobra`를 사용하여 CLI 커맨드 트리를 구성
- Root 커맨드에 서브커맨드(install, upgrade, list 등)를 등록
- 글로벌 플래그(`--debug`, `--namespace`)는 모든 서브커맨드에 전파

### 2. Action 패턴 (Configuration 공유)
- **실제 소스**: `pkg/action/action.go`
- `Configuration` 구조체가 모든 Action의 의존성을 보관
  - `RESTClientGetter`, `Releases(Storage)`, `KubeClient`, `RegistryClient`
- 각 Action(Install, Upgrade, List)은 `*Configuration`을 주입받음
- Action별 설정(DryRun, DisableHooks 등)은 별도 필드로 관리

### 3. 서브커맨드 실행 흐름
- **실제 소스**: `pkg/action/install.go`, `upgrade.go`, `list.go`
- `newInstallCmd(actionConfig)` → Install Action 생성 → `Run()` 호출
- upgrade --install 패턴: 릴리스가 없으면 install로 폴백

## 아키텍처 다이어그램

```
main() ─── NewRootCmd(actionConfig) ─── Execute(args)
                │                            │
                │  ┌─────────────┐    args[0] 매칭
                ├──│ installCmd  │────── Install.Run()
                │  └─────────────┘           │
                │  ┌─────────────┐           ▼
                ├──│ upgradeCmd  │──── Configuration
                │  └─────────────┘    (공유 의존성)
                │  ┌─────────────┐           │
                └──│ listCmd     │           ▼
                   └─────────────┘    ReleaseStorage
```

## 실행 방법

```bash
# 데모 모드 (모든 시나리오 자동 실행)
go run main.go

# CLI 모드
go run main.go install myapp ./mychart
go run main.go upgrade myapp ./mychart-v2
go run main.go list
go run main.go list --namespace production
go run main.go --debug install myapp ./mychart
```

## 실제 Helm 소스코드 참조

| 파일 | 역할 |
|------|------|
| `cmd/helm/helm.go` | main() 진입점, NewRootCmd 호출 |
| `pkg/cmd/root.go` | NewRootCmd, 서브커맨드 등록, Configuration 초기화 |
| `pkg/action/action.go` | Configuration 구조체, Action 공통 로직 |
| `pkg/action/install.go` | Install Action 구현 |
| `pkg/action/upgrade.go` | Upgrade Action 구현 |
| `pkg/action/list.go` | List Action 구현 |
