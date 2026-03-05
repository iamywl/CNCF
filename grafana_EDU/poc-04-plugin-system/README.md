# PoC 04: 플러그인 라이프사이클

## 개요

Grafana 플러그인 시스템의 전체 라이프사이클을 시뮬레이션한다.
Grafana는 Backend Plugin(Go 프로세스)과 Frontend Plugin(React 컴포넌트)을 지원하며,
Backend Plugin은 gRPC로 Grafana와 통신한다.

## 플러그인 라이프사이클

```
Discovery → Bootstrap → Initialization → Validation → Running → Shutdown
    │            │            │               │           │          │
    ▼            ▼            ▼               ▼           ▼          ▼
  파일시스템   메타데이터     Start()      서명 검증   QueryData   Stop()
  스캔        로드          gRPC 연결    권한 검사   CheckHealth  graceful
```

## 플러그인 상태

| 상태 | 설명 |
|------|------|
| NotStarted | 등록됨, 아직 시작되지 않음 |
| StartInit | 시작 요청됨, 초기화 중 |
| StartSuccess | 정상 동작 중 |
| StartFailed | 시작 실패 |
| Stopped | 정상 종료됨 |

## 플러그인 타입

| 타입 | 설명 | 예시 |
|------|------|------|
| datasource | 데이터소스 | Prometheus, Loki, MySQL |
| panel | 시각화 패널 | Graph, Table, Stat |
| app | 앱 플러그인 | Oncall, k6 |

## 시뮬레이션 내용

1. 플러그인 인터페이스 및 상태 머신
2. PluginRegistry: 등록, 조회, 목록
3. Discovery → Bootstrap → Init → Validation 파이프라인
4. gRPC 유사 통신 (채널 기반 RPC)
5. HealthCheck 메커니즘
6. Graceful shutdown

## 실행

```bash
go run main.go
```
