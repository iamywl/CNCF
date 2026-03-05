# PoC-06: Cilium 운영 시뮬레이션

## 개요

Cilium 에이전트의 운영 관련 핵심 메커니즘을 Go 표준 라이브러리만으로 시뮬레이션한다.
YAML 설정 파일 로딩/검증, 헬스 체크 엔드포인트, Prometheus 메트릭 수집 패턴을 재현한다.

## 소스 코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/option/config.go` | DaemonConfig (1400+ 필드), Validate(), ValidateUnchanged() |
| `pkg/option/config.go:1143` | HiveConfig{StartTimeout, StopTimeout, LogThreshold} |
| `pkg/option/constants.go` | 설정 상수, 기본값 정의 |
| `pkg/health/health_manager.go` | CiliumHealthManager, controllerInterval(60s), successfulPingTimeout(5m) |
| `pkg/health/server/server.go` | Health Server, Config, nodeMap |
| `pkg/health/server/prober.go` | prober -- ICMP/HTTP 프로빙, probeInterval |
| `pkg/health/server/status.go` | GetStatusHandler, PutStatusProbeHandler |
| `pkg/maps/metricsmap/metricsmap.go` | metricsmapCollector -- Prometheus Collector 구현 |
| `pkg/metrics/` | 메트릭 레지스트리, 네임스페이스(cilium_) 관리 |

## 시뮬레이션하는 핵심 개념

| 개념 | 실제 코드 | 시뮬레이션 |
|------|----------|-----------|
| 설정 로드 | `viper` + ConfigMap + 환경변수 + CLI | 간이 YAML 파서 + `DaemonConfig` |
| 설정 검증 | `DaemonConfig.Validate()` | 값 범위, 호환성, 필수 필드 검증 |
| 변경 감지 | `ValidateUnchanged()` (SHA256) | 체크섬 비교 |
| 헬스 체크 | `CiliumHealthManager` + `prober` | `HealthServer` -- 컴포넌트 + 노드 프로빙 |
| ICMP/HTTP 프로브 | `prober.probeNodes()` | 시뮬레이션된 프로브 결과 |
| 메트릭 수집 | `metricsmapCollector` + `pkg/metrics/` | `MetricRegistry` -- Counter, Gauge |
| Prometheus exposition | `/metrics` 엔드포인트 | text/plain exposition format |

## 5가지 시나리오

| # | 시나리오 | 검증 내용 |
|---|---------|----------|
| 1 | YAML 설정 로드/검증 | 파일 파싱, 기본값, 잘못된 설정 오류 보고 |
| 2 | 헬스 체크 | 컴포넌트 상태 갱신, 노드 ICMP/HTTP 프로빙, 전체 상태 |
| 3 | Prometheus 메트릭 | Counter/Gauge 등록, exposition format 출력 |
| 4 | HTTP 엔드포인트 | /healthz, /status, /metrics 실제 HTTP 요청 |
| 5 | 운영 아키텍처 요약 | ASCII 다이어그램으로 전체 구조 표현 |

## 실행 방법

```bash
cd cilium_EDU/poc-06-operations
go run main.go
```

프로그램이 `sample-config.yaml`을 자동 생성하고, 로드한 뒤 삭제한다.
실제 HTTP 서버도 임시로 시작되어 `/healthz`, `/metrics` 요청이 수행된다.

## 핵심 설계 원리

### 1. 설정 관리 (DaemonConfig)
- 실제 DaemonConfig는 1400개 이상의 필드를 가진 거대한 구조체이다.
- viper를 통해 YAML 파일, 환경변수(`CILIUM_` prefix), CLI 플래그에서 통합 로드한다.
- `Validate()`는 값 범위, 상호 호환성(예: native 라우팅 + tunnel=disabled)을 검증한다.
- `ValidateUnchanged()`는 SHA256 체크섬으로 런타임 설정 변경을 감지한다.

### 2. 헬스 체크 (cilium-health)
- `CiliumHealthManager`는 Hive Cell로 등록되어 에이전트 라이프사이클을 따른다.
- 60초 간격(`controllerInterval`)으로 클러스터 내 모든 노드를 프로빙한다.
- ICMP ping과 HTTP GET을 병렬로 수행하여 L3/L4/L7 연결성을 확인한다.
- 5분간 성공 없으면(`successfulPingTimeout`) health endpoint를 재시작한다.

### 3. Prometheus 메트릭
- `metricsmapCollector`는 BPF 메트릭 맵에서 데이터를 읽어 Prometheus 메트릭으로 변환한다.
- 드롭/포워딩 카운터를 방향(direction)과 사유(reason) 레이블로 구분한다.
- Per-CPU 값을 합산하여 cardinality를 낮게 유지한다 (line/file 정보는 노출하지 않음).
