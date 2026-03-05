# PoC 02: 대시보드 데이터 모델

## 개요

Grafana의 핵심 데이터 모델인 Dashboard, Panel, DataSource의 JSON 구조를 시뮬레이션한다.
대시보드는 Grafana UI의 최상위 단위이며, 패널들의 배치와 데이터소스 연결, 템플릿 변수를 관리한다.

## Grafana 대시보드 모델 구조

```
Dashboard
├── UID (고유 식별자)
├── Title
├── SchemaVersion (현재 39)
├── Version (낙관적 잠금용)
├── Time { From, To }
├── Templating
│   └── List[] { Name, Type, Query, Current }
└── Panels[]
    ├── ID, Title, Type (timeseries, stat, table, ...)
    ├── GridPos { X, Y, W, H }   ← 24컬럼 그리드
    ├── Targets[] (쿼리)
    │   ├── RefID ("A", "B", ...)
    │   ├── DatasourceUID
    │   └── Expr / Query
    └── Options (패널별 설정)
```

## 핵심 개념

| 개념 | 설명 |
|------|------|
| GridPos | 24컬럼 기반 레이아웃, GRID_CELL_HEIGHT=36px |
| SchemaVersion | 대시보드 스키마 버전, 마이그레이션에 사용 |
| Version | 낙관적 잠금 — 저장 시 버전 충돌 감지 |
| Template Variable | `$variable` 형태로 쿼리 내 동적 값 치환 |
| Provisioning | 파일 기반 대시보드 자동 배포 |

## 시뮬레이션 내용

1. Dashboard/Panel/DataSource 구조체 정의
2. 24컬럼 그리드 레이아웃 시뮬레이션
3. JSON 직렬화/역직렬화
4. 버전 충돌 감지 (낙관적 잠금)
5. 템플릿 변수 확장
6. 샘플 대시보드 생성 (3개 패널)

## 실행

```bash
go run main.go
```
