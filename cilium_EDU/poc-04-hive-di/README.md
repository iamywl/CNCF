# PoC-04: Cilium Hive DI 프레임워크 시뮬레이션

## 개요

Cilium의 Hive DI(의존성 주입) 프레임워크를 Go 표준 라이브러리만으로 재현한다.
`reflect.Type` 기반 의존성 해석, Cell/Module/Group 트리 구조, Lifecycle 역순 종료 등
실제 Hive의 핵심 메커니즘을 시뮬레이션한다.

## 소스 코드 참조

| 파일 | 역할 |
|------|------|
| `vendor/github.com/cilium/hive/hive.go` | Hive 컨테이너 — `dig.Container`, Start/Stop/Populate |
| `vendor/github.com/cilium/hive/cell/cell.go` | Cell 인터페이스, `container` 인터페이스 정의 |
| `vendor/github.com/cilium/hive/cell/module.go` | Module — `dig.Scope` 생성, `ModuleID`/`FullModuleID` |
| `vendor/github.com/cilium/hive/cell/provide.go` | Provide 셀 — 생성자 등록 (`export` 여부) |
| `vendor/github.com/cilium/hive/cell/invoke.go` | Invoke 셀 — 지연 실행 함수 등록 |
| `vendor/github.com/cilium/hive/cell/group.go` | Group 셀 — 스코프 없이 셀 묶음 |
| `vendor/github.com/cilium/hive/cell/lifecycle.go` | DefaultLifecycle — Hook 등록, 순서 시작/역순 종료 |
| `pkg/hive/hive.go` | Cilium 전용 래퍼 — ModuleDecorator, 기본 셀 추가 |

## 시뮬레이션하는 핵심 개념

| 개념 | 실제 코드 | 시뮬레이션 |
|------|----------|-----------|
| `cell.Provide(ctor)` | `provide.go` — `dig.Container.Provide()` | `Container.Provide()` + `reflect` 기반 타입 분석 |
| `cell.ProvidePrivate(ctor)` | `provide.go` — `export: false` | `Container.Provide(fn, false)` — 스코프 내부만 접근 |
| `cell.Invoke(fn)` | `invoke.go` — `InvokerList` 지연 실행 | `Hive.invokes` 수집 후 Populate에서 실행 |
| `cell.Module(id, desc, cells...)` | `module.go` — `dig.Scope` 생성 | `Container.Scope()` 중첩 |
| `cell.Group(cells...)` | `group.go` — 스코프 없이 묶음 | `GroupCell` — 직접 Apply |
| `cell.Lifecycle` | `lifecycle.go` — `DefaultLifecycle` | `Lifecycle` — Hook 등록/Start/Stop |
| `cell.Hook{OnStart, OnStop}` | `lifecycle.go:42` | `Hook` struct |
| `FullModuleID` | `module.go:41` — 중첩 모듈 경로 | `FullModuleID []string` |
| 의존성 해석 | `dig` 라이브러리 | `reflect.Type` 기반 스코프 체인 탐색 |

## 5가지 시나리오

| # | 시나리오 | 검증 내용 |
|---|---------|----------|
| 1 | 기본 Provide/Invoke/Lifecycle | reflect로 함수 시그니처 분석, 의존성 자동 해석, Hook 등록 |
| 2 | Module 중첩 스코프 + ProvidePrivate | 스코프 체인 (자식→부모 탐색), private 타입 격리 |
| 3 | 누락된 의존성 감지 | 존재하지 않는 타입 요구 시 ErrPopulate 반환 |
| 4 | Lifecycle 역순 종료 | DB→Cache→API→Health 시작 후 역순 종료 |
| 5 | 의존성 그래프 시각화 | ASCII 다이어그램으로 실제 Cilium 에이전트 구조 표현 |

## 실행 방법

```bash
cd cilium_EDU/poc-04-hive-di
go run main.go
```

## 핵심 설계 원리

### 1. reflect.Type 기반 의존성 해석
```
func NewEndpointManager(cfg *Config, ds *Datastore) *EndpointManager
                        ^^^^^^^^     ^^^^^^^^^^      ^^^^^^^^^^^^^^^^^
                        입력 타입 1   입력 타입 2      출력 타입
```
생성자 함수의 매개변수 타입이 "필요한 의존성", 반환값 타입이 "제공하는 객체"가 된다.
`reflect.TypeOf`로 타입을 추출하여 자동으로 의존성 그래프를 구축한다.

### 2. 스코프 체인
Module은 `dig.Scope`를 생성하여 이름 공간을 분리한다.
타입 해석 시 현재 스코프 → 부모 스코프 순서로 탐색한다.
`ProvidePrivate`로 등록된 타입은 부모 스코프에서 접근할 수 없다.

### 3. Lifecycle 역순 종료
Start는 등록 순서대로 실행하고, Stop은 역순으로 실행한다.
이유: 나중에 시작된 컴포넌트가 먼저 종료되어야, 의존하는 컴포넌트가
아직 살아있는 상태에서 안전하게 정리할 수 있다.

### 4. Invoke 지연 실행
`cell.Invoke`는 `Apply()` 시점에 실행되지 않고 `InvokerList`에 등록만 된다.
`Hive.Populate()` 시점에 비로소 실행되는데, 이는 설정 플래그가 먼저
등록/파싱되어야 하기 때문이다 (cobra 커맨드와의 통합).
