# PoC-12: Cobra CLI 구조

## 개요

Hubble CLI의 Cobra 기반 커맨드 트리 구조를 시뮬레이션한다. root 커맨드에서 observe, status, list, watch 서브커맨드로 분기하는 계층 구조, PersistentFlags 상속, 설정 우선순위(flag > env > config > default) 패턴을 재현한다.

## 핵심 개념

### 1. Command 트리 구조

```
hubble (root)
├── observe         ← Flow 이벤트 관찰
│   ├── flows       ← Flow 이벤트만 관찰
│   └── agent-events ← 에이전트 이벤트 관찰
├── status          ← 서버 상태 조회
├── list            ← 리소스 목록 조회
│   ├── nodes       ← 노드 목록
│   └── namespaces  ← 네임스페이스 목록
└── watch           ← 리소스 변경 감시
    └── peer        ← 피어 상태 스트리밍
```

### 2. PersistentFlags 상속

부모 커맨드의 PersistentFlags는 모든 자식 커맨드에서 사용 가능하다. root의 `--server`, `--tls`, `--debug` 플래그를 observe, status 등에서 모두 사용할 수 있다.

### 3. 설정 우선순위

Viper 패턴에 따라 flag > env > config > default 순서로 우선순위를 적용한다:

| 우선순위 | 소스 | 예시 |
|---------|------|------|
| 1 (최고) | CLI 플래그 | `--server 127.0.0.1:4245` |
| 2 | 환경 변수 | `HUBBLE_SERVER=...` |
| 3 | 설정 파일 | `~/.config/hubble/config.yaml` |
| 4 (최저) | 기본값 | `localhost:4245` |

### 4. PersistentPreRunE

커맨드 실행 전 검증 로직으로, 연결 초기화, 플래그 검증 등을 수행한다. 부모에서 자식으로 전파된다.

### 5. observe 커맨드 플래그

- **selector 플래그**: `--follow`, `--last`, `--first`, `--since`, `--until`, `--namespace`, `--verdict`
- **formatting 플래그**: `--output`, `--time-format`, `--print-node-name`, `--color`

## 실행 방법

```bash
go run main.go
```

설정 우선순위 데모와 여러 커맨드 실행 시뮬레이션을 순서대로 수행한다.

## 실제 소스코드 참조

| 파일 | 핵심 함수/구조체 |
|------|-----------------|
| `hubble/cmd/root.go` | `rootCmd` - 루트 커맨드, PersistentPreRunE |
| `hubble/cmd/observe/observe.go` | `observeCmd` - observe 서브커맨드, selectorFlags |
| `hubble/cmd/observe/flows.go` | `newFlowsCmd()` - flows 서브커맨드 |
| `hubble/cmd/status/status.go` | `statusCmd` - status 서브커맨드 |
| `hubble/cmd/list/list.go` | `listCmd` - list 서브커맨드 |
| `hubble/cmd/common/config/flags.go` | `GlobalFlags` - 전역 플래그 정의 |
| `hubble/cmd/common/conn/conn.go` | `conn.Init()` - 연결 초기화 |
