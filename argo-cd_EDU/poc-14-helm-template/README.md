# PoC 14: Helm 템플릿 시스템 (Helm Template Integration)

## 개요

Argo CD의 Helm 통합 레이어(`util/helm/`)를 Go 표준 라이브러리만으로 시뮬레이션합니다.

차트 로딩, Values 계층 병합, Go 템플릿 렌더링, 의존성 빌드, 동시 렌더링 잠금 등 Argo CD가 `helm template`를 실행하는 전체 과정을 구현합니다.

## 실행

```bash
go run main.go
```

## 핵심 개념

### 1. Helm 인터페이스

```go
type HelmEngine struct{}

func (e *HelmEngine) Template(chart *Chart, opts *TemplateOpts) ([]RenderedManifest, error)
func GetParameters(chart *Chart, opts *TemplateOpts) []HelmParameter
func DependencyBuild(chartPath string, chart *Chart) (*DependencyBuildResult, error)
```

### 2. TemplateOpts — 렌더링 옵션

```go
type TemplateOpts struct {
    ReleaseName string
    Namespace   string
    Values      map[string]interface{} // values.yaml 오버라이드
    SetParams   map[string]string      // --set 파라미터
    FileParams  map[string]string      // --set-file 파라미터
    KubeVersion string
    APIVersions []string
    SkipCRDs    bool
}
```

### 3. Values 우선순위 (낮음 → 높음)

```
차트 기본값 (values.yaml)
        ↓
사용자 values 파일 (argocd app values)
        ↓
--set 파라미터 (가장 높은 우선순위)
```

중첩 키 지원: `image.tag=v2.5.1` → `{"image": {"tag": "v2.5.1"}}`

### 4. 차트 구조

```
webapp/
├── Chart.yaml        # ChartMetadata (name, version, dependencies)
├── values.yaml       # 기본 Values
└── templates/
    ├── deployment.yaml
    ├── service.yaml
    └── configmap.yaml
```

### 5. DependencyBuild — 의존성 빌드

의존성 레포지토리 타입별 처리:

| 타입 | 형식 | 처리 방식 |
|------|------|-----------|
| OCI 레지스트리 | `oci://registry/repo` | OCI 레이어 추출 |
| HTTPS 레포지토리 | `https://charts.example.com` | .tgz 다운로드 |
| 로컬 | 경로 | 직접 참조 |

### 6. Marker 파일 — 중복 빌드 방지

```
chartPath/.argocd-helm-dep-up
```

마커 파일이 존재하면 의존성 빌드를 건너뜁니다. Git 동기화 후 새 커밋이 감지되면 마커가 삭제되어 의존성이 다시 빌드됩니다.

```
[1차 빌드] 의존성 다운로드 → 마커 파일 생성
[2차 빌드] 마커 파일 존재 확인 → 건너뜀
[새 커밋] 마커 파일 삭제 → 다시 빌드
```

### 7. manifestGenerateLock — 경로별 동시 렌더링 방지

```go
func TemplateWithLock(chartPath string, chart *Chart, opts *TemplateOpts) ([]RenderedManifest, error) {
    globalLockManager.Lock(chartPath)   // 경로별 뮤텍스 획득
    defer globalLockManager.Unlock(chartPath)
    // helm template 실행
}
```

동일 차트 경로에 대한 동시 `helm template` 실행을 방지하여 파일시스템 충돌을 예방합니다.

## 렌더링 파이프라인

```
TemplateOpts 입력
        │
        ▼
mergeValues(chart.Values, opts.Values)  ← 1단계: values.yaml 병합
        │
        ▼
applySetParams(merged, opts.SetParams)  ← 2단계: --set 적용
        │
        ▼
RenderContext 구성
  { .Release, .Chart, .Values }
        │
        ▼
템플릿 파일 순회 (정렬된 순서)
        │
        ▼
text/template.Execute(ctx)              ← Go 템플릿 렌더링
        │
        ▼
[]RenderedManifest 반환
```

## 지원하는 Helm 템플릿 함수

| 함수 | 설명 |
|------|------|
| `toYaml` | 값을 YAML 문자열로 변환 |
| `indent` | 지정 공백만큼 들여쓰기 |
| `nindent` | 줄바꿈 후 들여쓰기 |
| `quote` | 값을 따옴표로 감쌈 |
| `default` | 값이 없을 때 기본값 반환 |
| `required` | 필수 값 검증 |
| `trunc` | 문자열 자르기 |
| `lower`/`upper` | 대소문자 변환 |

## 시뮬레이션 시나리오

| 시나리오 | 내용 |
|----------|------|
| 1 | 기본 옵션으로 차트 렌더링 |
| 2 | Values 우선순위 — user values + --set |
| 3 | GetParameters — 유효 파라미터 목록 조회 |
| 4 | DependencyBuild + 마커 파일 중복 방지 |
| 5 | manifestGenerateLock — 동시 렌더링 직렬화 |
| 6 | OCI 레지스트리 의존성 처리 |

## 실제 Argo CD 코드 참조

| 구성요소 | 소스 위치 |
|----------|-----------|
| HelmTemplateOpts | `util/helm/cmd.go` |
| Template() | `util/helm/cmd.go` |
| DependencyBuild() | `util/helm/cmd.go` |
| GetParameters() | `util/helm/helm.go` |
| manifestGenerateLock | `reposerver/repository.go` |
| Marker 파일 | `reposerver/repository.go` |
