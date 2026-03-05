# PoC-13: 프린터/포매터 (Flow 출력)

## 개요

Hubble CLI의 Flow 출력 포맷터를 시뮬레이션한다. `Printer.WriteProtoFlow()`가 4가지 출력 포맷(Compact, Table, Dict, JSON)으로 Flow 이벤트를 렌더링하는 과정을 재현한다. ANSI 색상 코딩, IP-to-Pod 변환, reply 방향 반전 등 실제 Hubble CLI의 출력 로직을 포함한다.

## 핵심 개념

### 1. 4가지 출력 포맷

| 포맷 | CLI 옵션 | 설명 |
|------|---------|------|
| Compact | `-o compact` | 한 줄 요약, reply면 화살표 방전 |
| Table | `-o table` | tabwriter 기반 탭 정렬 테이블 |
| Dict | `-o dict` | KEY: VALUE 사전형, 구분선 분리 |
| JSON | `-o json` | JSON 직렬화 (proto3 매핑) |

### 2. ANSI 색상 코딩

verdict에 따라 다른 색상을 적용한다:
- **FORWARDED**: 초록색
- **DROPPED**: 빨간색
- **AUDIT**: 노란색
- **TRACED/TRANSLATED**: 노란색

Table 모드에서는 tabwriter와 ANSI 코드가 호환되지 않으므로 색상이 자동 비활성화된다.

### 3. IP-to-Pod 변환

`enableIPTranslation` 옵션이 활성화되면 IP 주소 대신 `namespace/podName` 형식으로 출력한다. 이는 Cilium의 IP identity 매핑을 활용한 것이다.

### 4. Reply 방향 반전

Compact 모드에서 `IsReply=true`인 Flow는 source와 destination이 반전되고, 화살표가 `<-`로 변경된다. `IsReply=nil`이면 방향 불명으로 `<>`가 사용된다.

### 5. Functional Options 패턴

`WithOutput()`, `WithColor()`, `WithNodeName()` 등의 함수형 옵션으로 Printer를 구성한다.

## 실행 방법

```bash
go run main.go
```

8가지 출력 포맷 변형을 순서대로 시연한다: Compact, Compact+노드명, Table, Table+노드명, Dict, JSON, IP 변환 없음, 색상 없음.

## 실제 소스코드 참조

| 파일 | 핵심 함수/구조체 |
|------|-----------------|
| `hubble/pkg/printer/printer.go` | `Printer.WriteProtoFlow()` - Flow 출력 진입점 |
| `hubble/pkg/printer/options.go` | `Output` enum, `Options`, `Option` 함수 |
| `hubble/pkg/printer/color.go` | `colorer` - verdict별 ANSI 색상 |
| `hubble/pkg/printer/printer.go` | `Printer.Hostname()` - IP-to-Pod 변환 |
| `hubble/pkg/printer/printer.go` | `Printer.getVerdict()` - verdict 색상 적용 |
