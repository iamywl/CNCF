# PoC: Argo CD Deep Links & Badge Server 시뮬레이션

## 개요

Argo CD의 UI 보조 서브시스템인 Deep Links(동적 외부 링크 생성)와
Badge Server(SVG 상태 배지 생성)를 시뮬레이션한다.

## 대응하는 Argo CD 소스코드

| 이 PoC | Argo CD 소스 | 설명 |
|--------|-------------|------|
| `DeepLink` | `util/settings/settings.go` | DeepLink 설정 구조체 |
| `EvaluateDeepLinks()` | `server/deeplinks/deeplinks.go` | Deep Link 평가/렌더링 |
| `evaluateCondition()` | expr 라이브러리 래퍼 | 조건식 평가 |
| `GenerateBadge()` | `server/badge/badge.go` | SVG 배지 생성 |
| `BadgeHandler()` | `server/badge/badge.go` | HTTP 엔드포인트 |

## 구현 내용

### 1. Deep Links
- Go 템플릿 기반 URL 렌더링
- 4가지 컨텍스트 (resource, application, cluster, project)
- 조건식 평가 (expr 언어 시뮬레이션)

### 2. Badge Server
- Shields.io 스타일 SVG 배지 생성
- Health/Sync 상태별 색상 매핑
- 멀티 앱 종합 배지

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- Deep Links는 ConfigMap 설정으로 외부 시스템 링크를 동적 생성한다
- Badge Server는 캐싱 비활성화 헤더로 항상 최신 상태를 반환한다
- 조건식으로 특정 앱/리소스에서만 링크를 표시할 수 있다
