# PoC-19: LogCLI & Query Tee 시뮬레이션

## 개요

Loki의 LogCLI(`cmd/logcli/`)와 Query Tee(`cmd/querytee/`)의 핵심 개념을 시뮬레이션한다.

## 시뮬레이션 항목

| 개념 | 소스 참조 | 시뮬레이션 방법 |
|------|----------|---------------|
| 커맨드 디스패치 | `cmd/logcli/main.go` | kingpin 대신 자체 CLI 구조체 |
| HTTP/File 클라이언트 | `pkg/logcli/client/` | Client 인터페이스 + 두 구현체 |
| 병렬 쿼리 | `query.DoQueryParallel` | 시간 분할 + 워커 풀 |
| Query Tee 프록시 | `cmd/querytee/main.go` | 듀얼 백엔드 + 응답 비교 |
| 출력 포매터 | `pkg/logcli/output/` | default/raw/jsonl 포매터 |

## 실행

```bash
go run main.go
```

## 핵심 출력

- LogCLI 커맨드 디스패치 및 쿼리 실행
- stdin(File) 모드 로그 필터링
- 출력 모드별 포맷 비교
- 병렬 쿼리의 시간 범위 분할 및 워커 실행
- Query Tee 프록시의 듀얼 백엔드 요청 및 응답 비교 통계
