# PoC-13: Helm PostRenderer

## 개요

PostRenderer는 Helm이 매니페스트를 렌더링한 후, Kubernetes에 적용하기 전에 매니페스트를 변환할 수 있는 인터페이스이다. kustomize, envsubst 등 외부 도구를 파이프라인에 끼워넣거나, 라벨/주석을 일괄 추가하는 데 사용된다.

## 참조 소스코드

| 파일 | 설명 |
|------|------|
| `pkg/postrenderer/postrenderer.go` | PostRenderer 인터페이스, NewPostRendererPlugin |
| `pkg/action/action.go` | annotateAndMerge, splitAndDeannotate |

## 핵심 개념

### 1. PostRenderer 인터페이스
```
type PostRenderer interface {
    Run(renderedManifests *bytes.Buffer) (*bytes.Buffer, error)
}
```

### 2. 파이프라인 흐름
템플릿 렌더링 -> annotateAndMerge -> PostRenderer.Run -> splitAndDeannotate -> K8s 적용

### 3. annotateAndMerge / splitAndDeannotate
- 여러 템플릿 파일 출력을 `# Source:` 주석과 함께 하나로 병합
- PostRenderer 처리 후 다시 파일별로 분리

### 4. Helm v4 Plugin PostRenderer
- `type: postrenderer/v1` 플러그인으로 확장 가능
- Plugin.Invoke로 매니페스트 전달/수신

## 실행

```bash
go run main.go
```

## 시뮬레이션 내용

1. PostRenderer 인터페이스 소개
2. LabelInjector, AnnotationInjector, ImageRewriter, NamespaceOverride 구현
3. PostRenderer 체이닝 (파이프라인)
4. annotateAndMerge / splitAndDeannotate 라운드트립
5. 빈 출력 검사
6. 전체 아키텍처 다이어그램
