# PoC-14: Helm 플러그인 시스템

## 개요

Helm의 플러그인 시스템은 CLI 확장, 커스텀 다운로더, 렌더링 후처리 등을 위한 확장 메커니즘을 제공한다. Helm v4에서는 apiVersion v1 형식의 plugin.yaml과 WebAssembly 런타임이 추가되었다.

## 참조 소스코드

| 파일 | 설명 |
|------|------|
| `internal/plugin/plugin.go` | Plugin 인터페이스, Input/Output, validPluginName |
| `internal/plugin/loader.go` | LoadDir, LoadAll, FindPlugin, detectDuplicates |
| `internal/plugin/metadata.go` | Metadata, fromMetadataLegacy, fromMetadataV1 |

## 핵심 개념

### 1. plugin.yaml 구조 (v1)
- `apiVersion: v1`
- `type`: cli/v1, getter/v1, postrenderer/v1
- `runtime`: subprocess, extism/v1 (wasm)

### 2. 플러그인 탐색
- `HELM_PLUGINS` 환경변수 (기본: `~/.local/share/helm/plugins`)
- `basedir/*/plugin.yaml` 패턴으로 스캔
- 중복 이름 검출

### 3. 환경변수 전달
- `HELM_PLUGIN_DIR`, `HELM_PLUGIN_NAME`, `HELM_BIN`
- `HELM_NAMESPACE`, `HELM_DEBUG` 등

### 4. 런타임
- subprocess: os/exec로 외부 프로세스 실행
- extism/v1: WebAssembly 런타임

## 실행

```bash
go run main.go
```

## 시뮬레이션 내용

1. plugin.yaml 구조 비교 (v1 vs legacy)
2. 플러그인 등록 및 타입/이름별 검색
3. 플러그인 실행 (환경변수 전달)
4. 플러그인 이름 유효성 검사
5. 중복 플러그인 검출
6. 디렉토리 구조 및 탐색 흐름 다이어그램
