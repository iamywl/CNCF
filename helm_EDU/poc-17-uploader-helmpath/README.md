# PoC-17: Uploader, Helmpath, Version Management 시뮬레이션

## 개요

Helm의 ChartUploader(`pkg/uploader/`), Helmpath(`pkg/helmpath/`), Version Management(`internal/version/`)의 핵심 개념을 시뮬레이션한다.

## 시뮬레이션 항목

| 개념 | 소스 참조 | 시뮬레이션 방법 |
|------|----------|----------------|
| ChartUploader | `pkg/uploader/chart_uploader.go` | 전략 패턴 기반 프로토콜별 업로드 |
| Pusher Provider 체인 | `pkg/uploader/chart_uploader.go` | scheme 기반 Pusher 선택 |
| 함수형 옵션 패턴 | `pkg/uploader/chart_uploader.go` | PushOption 함수형 옵션 |
| lazypath | `pkg/helmpath/lazypath.go` | XDG 기반 3단계 경로 해석 |
| 환경 변수 우선순위 | `pkg/helmpath/lazypath.go` | Helm > XDG > OS 기본값 |
| OS별 기본 경로 | `pkg/helmpath/lazypath_darwin.go` | darwin/linux/windows 분기 |
| CacheIndexFile | `pkg/helmpath/home.go` | 캐시 파일 경로 생성 |
| BuildInfo | `internal/version/version.go` | 버전 정보 구조체 |
| client-go 매핑 | `internal/version/clientgo.go` | v0.X → v1.X 변환 |
| UserAgent | `internal/version/version.go` | Helm/{version} 형식 |

## 실행

```bash
go run main.go
```

## 핵심 출력

- ChartUploader의 프로토콜별 업로드 (OCI 스킴)
- 스킴 누락 및 미지원 프로토콜 오류 처리
- Helmpath의 XDG 3단계 우선순위 경로 해석
- 환경 변수 변경에 따른 동적 경로 변화
- 캐시 인덱스/차트 파일 경로 생성
- BuildInfo 및 client-go 버전 매핑 테이블
- 메타데이터 포함 버전 및 UserAgent
- OS별 기본 경로 비교
