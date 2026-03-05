# PoC-04: Helm v4 Chart 로더

## 개요

Helm v4의 차트 로딩 매커니즘(디렉토리/아카이브 감지, Chart.yaml 파싱, 의존성 트리 구성)을 시뮬레이션합니다.

## 시뮬레이션하는 패턴

| 패턴 | 실제 소스 | 설명 |
|------|----------|------|
| ChartLoader 인터페이스 | `pkg/chart/loader/load.go` | Load() 메서드 |
| Loader 팩토리 | `loader.Loader()` | os.Stat → DirLoader/FileLoader 선택 |
| API 버전 감지 | `loader.LoadDir/LoadFile` | chartBase.APIVersion → v1/v2/v3 |
| 디렉토리 로드 | `v2/loader/load.go` | Chart.yaml→values.yaml→templates/→charts/ |
| .helmignore | `pkg/ignore/rules.go` | .gitignore 스타일 파일 제외 패턴 |

## 실행 방법

```bash
go run main.go
```

## 차트 디렉토리 구조

```
myapp/
├── Chart.yaml          ← 메타데이터 (apiVersion, name, version, dependencies)
├── values.yaml         ← 기본 설정값
├── templates/          ← Go 템플릿 파일
│   ├── _helpers.tpl    ← 파셜 (직접 렌더링 안됨)
│   └── deployment.yaml
├── charts/             ← 서브차트 (의존성)
│   ├── redis/
│   │   ├── Chart.yaml
│   │   └── templates/
│   └── postgresql/
│       ├── Chart.yaml
│       └── templates/
└── .helmignore         ← 무시 패턴
```

## 로딩 흐름

```
Loader(path) ── os.Stat ──┬── IsDir? → DirLoader
                           └── IsFile? → FileLoader
                                          │
DirLoader.Load():                         ▼
  1. Chart.yaml 읽기 → API 버전 감지 (v1/v2/v3)
  2. Metadata 파싱 + 유효성 검사
  3. values.yaml 로드 → chart.Values
  4. templates/ 디렉토리 순회 → chart.Templates
  5. charts/ 하위 디렉토리 재귀 로드 → chart.dependencies
```
