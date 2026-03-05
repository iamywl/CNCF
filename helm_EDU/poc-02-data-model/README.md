# PoC-02: Helm v4 데이터 모델

## 개요

Helm v4의 핵심 데이터 구조체(Chart, Release, Values, Hook)를 Go 표준 라이브러리만으로 시뮬레이션합니다.

## 시뮬레이션하는 구조체

| 구조체 | 실제 소스 | 역할 |
|--------|----------|------|
| Values | `pkg/chart/common/values.go` | 중첩 맵, 점 표기법 경로 탐색 |
| Metadata | `pkg/chart/v2/metadata.go` | Chart.yaml 내용, 유효성 검사 |
| Chart | `pkg/chart/v2/chart.go` | 패키지 본체: 메타데이터+템플릿+의존성 트리 |
| Release | `pkg/release/v1/release.go` | 배포 인스턴스: 이름+리비전+상태+매니페스트 |
| Info | `pkg/release/v1/info.go` | 릴리스 메타정보 (시간, 상태, 노트) |
| Hook | `pkg/release/v1/hook.go` | 라이프사이클 훅 (이벤트, 가중치, 삭제 정책) |
| Status | `pkg/release/common/status.go` | 9가지 릴리스 상태 |

## 실행 방법

```bash
go run main.go
```

## 핵심 개념

### Values 경로 탐색
```
Values{"image": {"tag": "1.21"}}
  → PathValue("image.tag") = "1.21"
  → Table("image") = {"tag": "1.21"}
```

### Chart 의존성 트리
```
myapp v1.0.0 [application]
  redis v17.3.14 [application]
  postgresql v12.1.9 [application]
```

### Release 상태 머신
```
pending-install → deployed → pending-upgrade → superseded
                           → pending-rollback
                           → uninstalling → uninstalled
                           → failed
```
