# PoC 03: 쿼리 파이프라인

## 개요

Grafana의 쿼리 실행 파이프라인을 시뮬레이션한다.
대시보드 패널이 데이터를 표시하려면 데이터소스에 쿼리를 보내고, 결과를 DataFrame으로 변환하여 반환한다.
이 파이프라인은 Grafana의 가장 핵심적인 데이터 흐름이다.

## 쿼리 파이프라인 흐름

```
Frontend Panel
    │
    ▼
/api/ds/query  (POST)
    │
    ▼
QueryService.QueryData()
    │
    ├── 쿼리 그룹핑 (데이터소스별)
    ├── 권한 검사
    ├── 타임아웃 설정
    │
    ▼
DataSourcePlugin.QueryData(ctx, req)
    │
    ├── Prometheus: HTTP API 호출 → range_query
    ├── Loki: HTTP API 호출 → query_range
    └── MySQL: SQL 실행 → rows
    │
    ▼
DataFrame 변환
    │
    ├── Fields: [Time, Value, Labels]
    ├── Meta: { ExecutedQueryString, Stats }
    │
    ▼
QueryDataResponse (RefID별 응답)
```

## 핵심 데이터 구조

| 구조체 | 설명 |
|--------|------|
| DataQuery | 단일 쿼리 요청 (RefID, DatasourceUID, Expr) |
| DataFrame | 결과 데이터 프레임 (Name, Fields, Meta) |
| Field | 데이터 필드 (Name, Type, Values) |
| QueryDataRequest | 쿼리 요청 묶음 |
| QueryDataResponse | RefID → DataFrame 매핑 |

## 실행

```bash
go run main.go
```
