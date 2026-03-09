# PoC: Argo CD Multi-Source Applications & UI 시뮬레이션

## 개요

Argo CD의 Multi-Source Applications(다중 소스 앱 관리)와
React 기반 웹 대시보드(UI) 구조를 시뮬레이션한다.

## 대응하는 Argo CD 소스코드

| 이 PoC | Argo CD 소스 | 설명 |
|--------|-------------|------|
| `ApplicationSource` | `pkg/apis/application/v1alpha1/types.go` | 소스 데이터 모델 |
| `GetSources()` | `types.go` ApplicationSpec 메서드 | Source/Sources 통합 |
| `ResolveRef()` | reposerver 내 Ref 해석 로직 | $ref/path 해석 |
| `GenerateManifests()` | `reposerver/repository/repository.go` | 매니페스트 생성 |
| `UIRoutes` | `ui/src/app/` 라우팅 | React SPA 라우팅 |
| `ResourceNode` | 앱 상세 페이지 리소스 트리 | 리소스 트리 시각화 |

## 구현 내용

### 1. Multi-Source
- Sources[] 다중 소스 설정
- Ref 소스로 values.yaml 참조 ($ref/path)
- 소스별 매니페스트 생성 (Helm/Kustomize/Plain YAML)
- 하위 호환성 (Source ↔ Sources)

### 2. UI 시뮬레이션
- React SPA 라우팅 테이블
- 리소스 트리 시각화

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- Multi-Source는 Helm 차트 + 별도 values 리포 + 추가 매니페스트를 하나의 앱으로 관리한다
- Ref 소스는 매니페스트를 생성하지 않고, 다른 소스에서 $ref로 참조하는 값만 제공한다
- GetSources() 메서드로 단일/다중 소스를 투명하게 처리한다
