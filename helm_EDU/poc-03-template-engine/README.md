# PoC-03: Helm v4 템플릿 엔진

## 개요

Helm v4의 Go `text/template` 기반 템플릿 렌더링 엔진을 시뮬레이션합니다.

## 시뮬레이션하는 패턴

| 패턴 | 실제 소스 | 설명 |
|------|----------|------|
| 렌더링 엔진 | `pkg/engine/engine.go` | text/template 기반, 빈 부모→Parse→ExecuteTemplate |
| 커스텀 함수 | `pkg/engine/funcs.go` | toYaml, toJson, upper, default, indent, quote 등 |
| include 함수 | `engine.go:includeFun` | 재귀 방지 카운터 포함 이름 있는 템플릿 참조 |
| 값 스코핑 | `engine.go:recAllTpls` | 서브차트별 Values 섹션 분리 |
| required | `engine.go:initFunMap` | 필수 값 누락시 에러 |

## 실행 방법

```bash
go run main.go
```

## 렌더링 흐름

```
Chart.Templates + Values
        │
        ▼
Engine.Render()
  ├── collectTemplates() ← allTemplates/recAllTpls
  │     └── 값 스코핑: 서브차트별 Values 분리
  ├── template.New("gotpl")
  ├── initFunMap() ← 커스텀 함수 등록
  ├── Parse (모든 템플릿)
  └── ExecuteTemplate (파셜 제외)
        │
        ▼
  map[string]string (파일명 → 렌더링된 YAML)
```

## 핵심 커스텀 함수

| 함수 | 용도 | 예시 |
|------|------|------|
| `toYaml` | 값→YAML 문자열 | `{{ toYaml .Values.labels \| indent 4 }}` |
| `include` | 다른 템플릿 참조 | `{{ include "app.labels" . }}` |
| `default` | 기본값 | `{{ default "latest" .Values.tag }}` |
| `required` | 필수값 검사 | `{{ required "need password" .Values.pw }}` |
| `upper/lower` | 대소문자 변환 | `{{ upper .Values.name }}` |
| `indent/nindent` | 들여쓰기 | `{{ include "labels" . \| nindent 4 }}` |
