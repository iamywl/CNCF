# PoC 01: Wire DI 시뮬레이션

## 개요

Grafana는 Google Wire를 사용하여 컴파일 타임 의존성 주입(Dependency Injection)을 수행한다.
이 PoC는 Wire의 핵심 메커니즘을 표준 라이브러리만으로 재현한다.

## Grafana에서 Wire 사용 방식

Grafana의 `pkg/server/wire.go`에서 `wire.Build()`를 호출하여 서버의 모든 의존성을 연결한다.
Provider 함수들이 의존성 그래프를 형성하고, Wire가 이를 위상 정렬하여 올바른 초기화 순서를 결정한다.

### 핵심 개념

| 개념 | 설명 |
|------|------|
| Provider | 의존성을 생성하는 함수 (예: `ProvideStore(logger Logger) Store`) |
| Injector | Provider들을 조합하여 최종 객체를 생성하는 함수 |
| wire.Build | Provider 목록을 받아 의존성 그래프를 구성 |
| wire.Bind | 인터페이스를 구체 타입에 바인딩 |
| ProviderSet | 관련 Provider들의 그룹 |

## 시뮬레이션 내용

1. **서비스 인터페이스 정의**: Logger, Store, HTTPServer
2. **Provider 함수**: 각 서비스의 생성자 함수
3. **의존성 그래프 구축**: Provider의 입출력 타입을 분석하여 그래프 생성
4. **위상 정렬**: 초기화 순서 결정 (Kahn's algorithm)
5. **순환 의존성 감지**: DFS로 사이클 탐지
6. **인터페이스 바인딩**: wire.Bind 시뮬레이션

## 실행

```bash
go run main.go
```

## 출력 예시

```
=== Wire DI Container Simulation ===
[Provider 등록] Logger provider 등록됨
[Provider 등록] Store provider 등록됨 (의존: Logger)
[Provider 등록] HTTPServer provider 등록됨 (의존: Store, Logger)
[의존성 그래프] 위상 정렬 결과: Logger → Store → HTTPServer
[빌드 완료] 모든 서비스 초기화 성공
```
