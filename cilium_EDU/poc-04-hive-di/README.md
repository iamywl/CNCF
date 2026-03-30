# PoC-04: Cilium Hive DI 프레임워크 시뮬레이션

## 개요

Cilium의 **Hive DI(의존성 주입) 프레임워크**를 Go 표준 라이브러리(`reflect`, `sync`, `context`)만으로 재현한다. `reflect.Type` 기반 의존성 해석, Cell/Module/Group 트리 구조, Lifecycle 역순 종료, Invoke 지연 실행 등 핵심 메커니즘 5가지를 시뮬레이션한다.

## 배경 지식

### 의존성 주입(DI)이란?

의존성 주입은 객체가 자신의 의존성을 직접 생성하지 않고, 외부에서 주입받는 설계 패턴이다.
`NewEndpointManager(ds *Datastore)`처럼 생성자 매개변수로 의존성을 선언하면,
DI 프레임워크가 자동으로 찾아서 넣어준다. 하드코딩된 `NewDatastore()` 호출이 사라지므로
테스트와 교체가 쉬워진다.

### Cilium이 Hive를 만든 이유

Cilium은 초기에 **uber-go/dig**를 DI 프레임워크로 사용했다. 하지만 프로젝트 규모가 커지면서
dig만으로는 부족한 기능이 필요해졌다:

| 문제 | dig의 한계 | Hive의 해결 |
|------|-----------|------------|
| 모듈 구조화 | 플랫한 컨테이너 | `Cell` 트리 + `Module` 스코프 |
| 생명주기 관리 | 없음 | `Lifecycle` + `Hook{OnStart, OnStop}` |
| 설정 통합 | 없음 | `cell.Config` + cobra 플래그 자동 등록 |
| 의존성 가시성 | 전역 | `ProvidePrivate`로 스코프 내부 격리 |
| 지연 실행 | 즉시 실행 | `Invoke` → `Populate` 시점까지 지연 |

Hive는 dig 위에 `Cell` 추상화를 쌓아서, 100개 이상의 서브시스템으로 구성된 Cilium 에이전트를
**선언적으로** 조립할 수 있게 한다.

### Provide/Invoke 패턴과 스코프 체인

- **Provide**: 생성자 함수를 등록한다. 매개변수 타입 = 의존성, 반환값 타입 = 제공 객체.
- **Invoke**: 부수 효과(Lifecycle Hook 등록 등)를 `Populate()` 시점까지 지연 실행한다.
- **스코프 체인**: `Module`이 `dig.Scope`를 생성하여 이름 공간을 분리한다.
  타입 해석 시 현재 스코프 -> 부모 스코프 순서로 탐색하며,
  `ProvidePrivate` 타입은 해당 스코프 안에서만 접근 가능하다.
- **reflect 기반 해석**: `reflect.TypeOf`로 생성자의 시그니처를 런타임에 분석하여
  자동으로 의존성 그래프를 구축한다.

## 시뮬레이션하는 핵심 개념

| 개념 | 실제 Cilium 코드 | PoC 구현 |
|------|-----------------|----------|
| `cell.Provide(ctor)` | `provide.go` - `dig.Container.Provide()` | `Container.Provide()` + `reflect` 기반 타입 분석 |
| `cell.ProvidePrivate(ctor)` | `provide.go` - `export: false` | `Container.Provide(fn, false)` - 스코프 내부만 접근 |
| `cell.Invoke(fn)` | `invoke.go` - `InvokerList` 지연 실행 | `Hive.invokes` 수집 후 `Populate`에서 실행 |
| `cell.Module(id, desc, cells...)` | `module.go` - `dig.Scope` 생성 | `Container.Scope()` 중첩 |
| `cell.Group(cells...)` | `group.go` - 스코프 없이 묶음 | `GroupCell` - 직접 Apply |
| `cell.Lifecycle` | `lifecycle.go` - `DefaultLifecycle` | `Lifecycle` - Hook 등록/Start/Stop |
| `FullModuleID` | `module.go:41` - 중첩 모듈 경로 | `FullModuleID []string` |
| 의존성 해석 | `dig` 라이브러리 내부 | `reflect.Type` 기반 스코프 체인 탐색 |

## 아키텍처 다이어그램

```
┌─────────────────────────────────────────────────────────────────┐
│  Hive                                                           │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  Container (root)                                        │   │
│  │  providers: {*Config -> NewConfig, *Datastore -> NewDS}  │   │
│  │                                                          │   │
│  │  ┌─────────────────────────────────────┐                 │   │
│  │  │  Scope: "controlplane"              │                 │   │
│  │  │  providers: {*EndpointMgr -> New}   │                 │   │
│  │  │                                     │                 │   │
│  │  │  ┌──────────────────────────┐       │                 │   │
│  │  │  │  Scope: "endpoint-mgr"   │       │                 │   │
│  │  │  │  private: {*cache}       │       │                 │   │
│  │  │  └──────────────────────────┘       │                 │   │
│  │  └─────────────────────────────────────┘                 │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                 │
│  Lifecycle: [Hook1(Start/Stop), Hook2(Start/Stop), ...]         │
│  invokes:   [registerHooks, initMetrics, ...]                   │
└─────────────────────────────────────────────────────────────────┘

Resolve 흐름 (스코프 체인):

  endpoint-mgr 스코프에서 *Config 요청
       │
       ▼
  [endpoint-mgr] providers에 *Config 있나? → 없음
       │
       ▼
  [controlplane] providers에 *Config 있나? → 없음
       │
       ▼
  [root] providers에 *Config 있나? → 있음! → NewConfig() 호출 → 반환
```

## 코드 해설

### 1. `Container` - DI 컨테이너 핵심

`dig.Container`와 `dig.Scope`를 하나의 구조체로 통합했다. `parent` 포인터로
스코프 체인을 형성하며, `Resolve()` 시 현재 -> 부모 순서로 탐색한다.
`instances` 맵에 한 번 생성한 객체를 캐싱하여 싱글턴 패턴을 구현한다.

### 2. `newCtor()` - reflect 기반 생성자 분석

`reflect.TypeOf(fn)`으로 함수의 매개변수 타입(의존성)과 반환값 타입(제공 객체)을 추출한다.
마지막 반환값이 `error`이면 에러 처리용으로 분리한다.
이것이 "선언만 하면 자동 와이어링"의 핵심이다.

### 3. `Lifecycle` - 생명주기 관리

`Start()`는 등록 순서대로, `Stop()`은 **역순**으로 실행한다.
나중에 시작된 컴포넌트가 먼저 종료되어야, 의존 대상이 아직 살아있는 상태에서
안전하게 정리할 수 있기 때문이다.

### 4. `Hive.applyCell()` - 재귀적 셀 적용

Cell 트리를 재귀 순회하면서 `ModuleCell`은 새 스코프를 생성하고,
`InvokeCell`의 함수는 `invokes` 슬라이스에 수집만 해둔다.
`ProvideCell`은 즉시 `Container.Provide()`로 생성자를 등록한다.

### 5. `Hive.Populate()` - 객체 그래프 인스턴스화

수집된 Invoke 함수들을 실행하여 실제 객체를 생성하고 Lifecycle Hook을 등록한다.
cobra 플래그 파싱이 완료된 후 실행되므로, 설정값이 확정된 상태에서 초기화된다.

## 실행 방법

```bash
cd cilium_EDU/poc-04-hive-di
go run main.go
```

5가지 시나리오가 순서대로 실행된다:

| # | 시나리오 | 검증 내용 |
|---|---------|----------|
| 1 | 기본 Provide/Invoke/Lifecycle | reflect 타입 분석, 의존성 자동 해석, Hook 등록 |
| 2 | Module 중첩 스코프 + ProvidePrivate | 스코프 체인 탐색, private 타입 격리 확인 |
| 3 | 누락된 의존성 감지 | 존재하지 않는 타입 요구 시 에러 반환 |
| 4 | Lifecycle 역순 종료 | DB->Cache->API->Health 시작, 역순 종료 |
| 5 | 의존성 그래프 시각화 | ASCII 다이어그램으로 에이전트 구조 표현 |

## 핵심 포인트

1. **reflect.Type이 의존성 그래프의 키이다** - 생성자 함수의 시그니처만으로 자동 와이어링된다.
   개발자는 "무엇을 제공하고, 무엇이 필요한지"만 선언하면 된다.

2. **스코프 체인으로 캡슐화를 달성한다** - Module은 dig.Scope를 생성하여 이름 공간을 분리하고,
   ProvidePrivate는 스코프 밖에서의 접근을 차단한다.

3. **Lifecycle 역순 종료는 의존성 안전성의 핵심이다** - LIFO(후입선출) 순서로 종료하면
   의존 대상이 아직 살아있는 상태에서 정리할 수 있다.

4. **Invoke 지연 실행은 설정 통합을 가능하게 한다** - cobra 플래그 파싱이 완료된 후에야
   실제 객체를 생성하므로, 설정값이 확정된 상태에서 초기화가 진행된다.

5. **Cell 트리로 100개 이상의 서브시스템을 선언적으로 조립한다** - Cilium 에이전트의
   `daemon/cmd/daemon.go`에서 `hive.New(cells...)`로 전체 시스템을 구성한다.

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium Hive | 이 PoC |
|------|-----------------|--------|
| DI 엔진 | uber-go/dig (성숙한 라이브러리) | `reflect` 직접 사용 (교육 목적) |
| 설정 통합 | `cell.Config` + cobra 플래그 자동 등록 | 미구현 |
| ModuleDecorator | 모듈별 스코프된 로거/메트릭 자동 주입 | 미구현 |
| Health 체크 | `cell.Health` + 모듈별 상태 리포팅 | 미구현 |
| 순환 의존성 | dig가 컴파일 타임에 감지 | 런타임 무한루프 가능 |
| 에러 리포팅 | dig의 상세한 에러 메시지 + dot 그래프 출력 | 기본 에러 메시지만 |
| 동시성 | Start 훅 순차 실행 (의존성 순서 보장) | 동일 |
| Shutdowner | 시그널 핸들링 + graceful shutdown | 미구현 |

## 소스 코드 참조

- `vendor/github.com/cilium/hive/hive.go` - Hive 컨테이너 (Start/Stop/Populate)
- `vendor/github.com/cilium/hive/cell/cell.go` - Cell 인터페이스
- `vendor/github.com/cilium/hive/cell/module.go` - Module (dig.Scope, FullModuleID)
- `vendor/github.com/cilium/hive/cell/provide.go` - Provide 셀 (생성자 등록)
- `vendor/github.com/cilium/hive/cell/invoke.go` - Invoke 셀 (지연 실행)
- `vendor/github.com/cilium/hive/cell/group.go` - Group 셀 (스코프 없는 묶음)
- `vendor/github.com/cilium/hive/cell/lifecycle.go` - DefaultLifecycle (Hook 관리)
- `pkg/hive/hive.go` - Cilium 전용 래퍼 (ModuleDecorator)
