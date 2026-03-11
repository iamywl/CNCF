# PoC-29: Grafana Library Panel 시뮬레이션

## 개요

Library Panel은 여러 대시보드에서 공유 가능한 재사용 패널이다.
이 PoC는 Library Element CRUD, 대시보드 연결, 변경 전파, 버전 이력을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 Grafana 코드 | 시뮬레이션 |
|------|------------------|-----------|
| Library Element | `pkg/services/libraryelements/` | 공유 패널 생성/수정/삭제 |
| Connected Dashboards | 연결된 대시보드 추적 | 변경 시 전파 |
| Version History | 버전 관리 | 변경 이력 저장 |
| Unlink | 연결 해제 | 독립 패널로 전환 |

## 실행 방법

```bash
cd grafana_EDU/poc-29-library-panel
go run main.go
```
